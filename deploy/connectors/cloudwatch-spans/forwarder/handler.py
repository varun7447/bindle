"""Forwards AWS service spans from CloudWatch Logs (aws/spans) to Bindle.

A CloudWatch Logs subscription filter on the aws/spans log group invokes this
function with batches of span records. Each record is one span as JSON. The
function keeps spans whose name starts with one of SPAN_NAME_PREFIXES,
converts them to OTLP/JSON and POSTs them, gzipped, to BINDLE_ENDPOINT.

The aws/spans record format is only partly documented, so conversion accepts
the variants seen in OpenTelemetry tooling: flat or nested attribute maps or
OTLP key/value lists, string or numeric span kinds and status codes, X-Ray or
hex trace IDs, and end times given directly or as a duration.

Environment:
  BINDLE_ENDPOINT            https://ingest.example.com (no path)
  BINDLE_API_KEY_SECRET_ARN  Secrets Manager secret holding the ingest key
  SPAN_NAME_PREFIXES         comma-separated, default "AgentCore."
  BINDLE_ALLOW_HTTP          "1" to allow a plain-http endpoint (tests only)

Standard library only, plus boto3 from the Lambda runtime for the secret.
"""

import base64
import gzip
import json
import os
import time
import urllib.error
import urllib.request

BATCH = 500
ATTEMPTS = 3
TIMEOUT_S = 10

_key_cache = {}

KINDS = {"INTERNAL": 1, "SERVER": 2, "CLIENT": 3, "PRODUCER": 4, "CONSUMER": 5, "UNSPECIFIED": 0}
STATUSES = {"UNSET": 0, "OK": 1, "ERROR": 2}


# ---------------------------------------------------------------------------
# Decoding the subscription payload


def decode_event(event):
    """Returns the log message strings in a CloudWatch Logs subscription event."""
    data = event.get("awslogs", {}).get("data")
    if not data:
        return []
    doc = json.loads(gzip.decompress(base64.b64decode(data)))
    if doc.get("messageType") != "DATA_MESSAGE":
        return []  # CONTROL_MESSAGE is CloudWatch checking the destination
    return [e.get("message", "") for e in doc.get("logEvents", [])]


def parse_spans(messages, prefixes):
    spans = []
    for m in messages:
        try:
            rec = json.loads(m)
        except ValueError:
            continue
        if isinstance(rec, dict) and str(rec.get("name", "")).startswith(prefixes):
            spans.append(rec)
    return spans


# ---------------------------------------------------------------------------
# Conversion to OTLP/JSON


def hex_id(v, length):
    """Normalizes a trace or span ID to lowercase hex of the given length."""
    if not v:
        return ""
    s = str(v).strip().lower()
    if s.startswith("1-") and s.count("-") == 2:  # X-Ray trace ID: 1-<8 hex>-<24 hex>
        s = s[2:].replace("-", "")
    s = s.replace("-", "")
    if len(s) != length or any(c not in "0123456789abcdef" for c in s):
        return ""
    return s


def flatten(obj, prefix=""):
    """Nested dicts become dotted keys; OTLP key/value lists become dicts."""
    if isinstance(obj, list) and all(isinstance(i, dict) and "key" in i for i in obj):
        obj = {i["key"]: otlp_value(i.get("value")) for i in obj}
    out = {}
    if not isinstance(obj, dict):
        return out
    for k, v in obj.items():
        key = f"{prefix}{k}"
        if isinstance(v, dict) and not is_otlp_value(v):
            out.update(flatten(v, key + "."))
        else:
            out[key] = otlp_value(v) if isinstance(v, dict) else v
    return out


def is_otlp_value(v):
    return isinstance(v, dict) and len(v) == 1 and next(iter(v)) in (
        "stringValue", "intValue", "doubleValue", "boolValue", "arrayValue", "kvlistValue", "bytesValue")


def otlp_value(v):
    """Unwraps an OTLP AnyValue ({"stringValue": "x"}) to a plain value."""
    if not is_otlp_value(v):
        return v
    (t, x), = v.items()
    if t == "intValue":
        return int(x)
    if t == "arrayValue":
        return [otlp_value(i) for i in (x or {}).get("values", [])]
    if t == "kvlistValue":
        return flatten((x or {}).get("values", []))
    return x


def any_value(v):
    if isinstance(v, bool):
        return {"boolValue": v}
    if isinstance(v, int):
        return {"intValue": str(v)}
    if isinstance(v, float):
        return {"doubleValue": v}
    if isinstance(v, (list, tuple)):
        return {"arrayValue": {"values": [any_value(i) for i in v]}}
    if v is None:
        return {"stringValue": ""}
    if isinstance(v, dict):
        return {"stringValue": json.dumps(v, separators=(",", ":"))}
    return {"stringValue": str(v)}


def key_values(attrs):
    return [{"key": k, "value": any_value(v)} for k, v in sorted(attrs.items())]


def enum(v, table, prefix):
    if isinstance(v, int):
        return v
    s = str(v or "").upper()
    if s.isdigit():
        return int(s)
    return table.get(s.removeprefix(prefix), 0)


def nanos(v):
    """Accepts unix nanoseconds as int or string; seconds or millis as a float or small int."""
    if v in (None, ""):
        return 0
    try:
        f = float(v)
    except (TypeError, ValueError):
        return 0
    if f < 1e11:  # seconds
        return int(f * 1e9)
    if f < 1e14:  # milliseconds
        return int(f * 1e6)
    if f < 1e17:  # microseconds
        return int(f * 1e3)
    return int(v) if isinstance(v, (int, str)) and str(v).isdigit() else int(f)


def first(rec, *keys):
    for k in keys:
        if rec.get(k) not in (None, ""):
            return rec[k]
    return None


def convert_span(rec):
    """One aws/spans record to an OTLP span dict, or None if it lacks IDs."""
    trace_id = hex_id(first(rec, "traceId", "trace_id"), 32)
    span_id = hex_id(first(rec, "spanId", "span_id"), 16)
    if not trace_id or not span_id:
        return None
    start = nanos(first(rec, "startTimeUnixNano", "startTime", "start_time"))
    end = nanos(first(rec, "endTimeUnixNano", "endTime", "end_time"))
    if not end and start:
        dur = first(rec, "durationNano", "duration_nano")
        end = start + int(float(dur)) if dur is not None else start
    status = rec.get("status") or {}
    if not isinstance(status, dict):
        status = {"code": status}
    span = {
        "traceId": trace_id,
        "spanId": span_id,
        "name": str(rec.get("name", "")),
        "kind": enum(rec.get("kind"), KINDS, "SPAN_KIND_"),
        "startTimeUnixNano": str(start),
        "endTimeUnixNano": str(end),
        "attributes": key_values(flatten(rec.get("attributes") or {})),
        "status": {"code": enum(status.get("code"), STATUSES, "STATUS_CODE_")},
    }
    parent = hex_id(first(rec, "parentSpanId", "parent_span_id"), 16)
    if parent:
        span["parentSpanId"] = parent
    if status.get("message"):
        span["status"]["message"] = str(status["message"])
    events = []
    for e in rec.get("events") or []:
        if isinstance(e, dict):
            events.append({
                "name": str(e.get("name", "")),
                "timeUnixNano": str(nanos(first(e, "timeUnixNano", "time", "timestamp"))),
                "attributes": key_values(flatten(e.get("attributes") or {})),
            })
    if events:
        span["events"] = events
    return span


def to_otlp(records):
    """Groups converted spans by resource and scope into an OTLP export request."""
    groups = {}
    for rec in records:
        span = convert_span(rec)
        if span is None:
            continue
        res = rec.get("resource") or {}
        res_attrs = flatten(res.get("attributes", res) if isinstance(res, dict) else res)
        scope = rec.get("scope") or rec.get("instrumentationScope") or {}
        if not isinstance(scope, dict):
            scope = {"name": str(scope)}
        rkey = json.dumps(res_attrs, sort_keys=True, default=str)
        skey = (str(scope.get("name", "")), str(scope.get("version", "")))
        groups.setdefault(rkey, (res_attrs, {}))[1].setdefault(skey, []).append(span)
    resource_spans = []
    for res_attrs, scopes in groups.values():
        resource_spans.append({
            "resource": {"attributes": key_values(res_attrs)},
            "scopeSpans": [{"scope": {"name": n, "version": v}, "spans": s} for (n, v), s in scopes.items()],
        })
    return {"resourceSpans": resource_spans}


# ---------------------------------------------------------------------------
# Sending


class SendError(Exception):
    pass


def ingest_key(secret_arn, fetch=None):
    if secret_arn in _key_cache:
        return _key_cache[secret_arn]
    if fetch is None:
        import boto3

        fetch = lambda arn: boto3.client("secretsmanager").get_secret_value(SecretId=arn)["SecretString"]  # noqa: E731
    key = fetch(secret_arn).strip()
    _key_cache[secret_arn] = key
    return key


def post(url, key, body, sleep=time.sleep):
    data = gzip.compress(json.dumps(body, separators=(",", ":")).encode())
    req_headers = {
        "Content-Type": "application/json",
        "Content-Encoding": "gzip",
        "Authorization": f"Bearer {key}",
        "User-Agent": "bindle-cloudwatch-spans/1",
    }
    for attempt in range(1, ATTEMPTS + 1):
        req = urllib.request.Request(url, data=data, headers=req_headers, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=TIMEOUT_S) as resp:  # noqa: S310 (scheme checked in endpoint_url)
                resp.read()
                return
        except urllib.error.HTTPError as e:
            if e.code not in (429, 500, 502, 503, 504) or attempt == ATTEMPTS:
                raise SendError(f"Bindle ingest returned HTTP {e.code}") from None
            wait = retry_after(e.headers.get("Retry-After"), attempt)
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            if attempt == ATTEMPTS:
                raise SendError(f"Bindle ingest unreachable: {e}") from None
            wait = retry_after(None, attempt)
        sleep(wait)


def retry_after(header, attempt):
    try:
        return min(max(float(header), 0.0), 10.0)
    except (TypeError, ValueError):
        return min(0.5 * 2 ** (attempt - 1), 10.0)


def endpoint_url(env):
    base = env.get("BINDLE_ENDPOINT", "").strip().rstrip("/")
    if not base:
        raise SendError("BINDLE_ENDPOINT is not set")
    if not base.startswith("https://") and not (env.get("BINDLE_ALLOW_HTTP") == "1" and base.startswith("http://")):
        raise SendError("BINDLE_ENDPOINT must use https")
    return base + "/v1/traces"


def forward(event, env=None, fetch_secret=None, sleep=time.sleep):
    env = os.environ if env is None else env
    prefixes = tuple(p.strip() for p in env.get("SPAN_NAME_PREFIXES", "AgentCore.").split(",") if p.strip())
    records = parse_spans(decode_event(event), prefixes)
    if not records:
        return {"forwarded": 0}
    url = endpoint_url(env)
    key = ingest_key(env.get("BINDLE_API_KEY_SECRET_ARN", ""), fetch_secret)
    sent = 0
    for i in range(0, len(records), BATCH):
        body = to_otlp(records[i:i + BATCH])
        n = sum(len(s["spans"]) for r in body["resourceSpans"] for s in r["scopeSpans"])
        if n:
            post(url, key, body, sleep)
            sent += n
    return {"forwarded": sent}


def handler(event, context):
    # Raising makes Lambda retry the batch. Bindle writes spans by trace and
    # span ID, so a retried batch doesn't create duplicates.
    result = forward(event)
    print(json.dumps(result))
    return result
