import base64
import gzip
import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

import handler as h

TRACE = "6a1f0c2b9d3e4f5a6b7c8d9e0f1a2b3c"
SPAN = "1a2b3c4d5e6f7a8b"


def subscription_event(messages, kind="DATA_MESSAGE"):
    doc = {"messageType": kind, "logGroup": "aws/spans", "logEvents": [{"id": str(i), "message": m} for i, m in enumerate(messages)]}
    return {"awslogs": {"data": base64.b64encode(gzip.compress(json.dumps(doc).encode())).decode()}}


def gateway_span(**over):
    rec = {
        "traceId": TRACE,
        "spanId": SPAN,
        "parentSpanId": "0011223344556677",
        "name": "AgentCore.Gateway.InvokeTool",
        "kind": "SERVER",
        "startTimeUnixNano": 1731840000000000000,
        "durationNano": 25000000,
        "attributes": {"tool.name": "lookup_order", "gateway.id": "gw-1", "latency_ms": 25, "aws": {"operation": {"name": "CallTool"}}},
        "resource": {"attributes": {"service.name": "my-gateway", "cloud.provider": "aws"}},
        "scope": {"name": "AgentCore.Gateway"},
        "status": {"code": "OK"},
    }
    rec.update(over)
    return rec


def attrs(span):
    return {kv["key"]: kv["value"] for kv in span["attributes"]}


class ConvertTest(unittest.TestCase):
    def test_gateway_span(self):
        body = h.to_otlp([gateway_span()])
        rs = body["resourceSpans"][0]
        self.assertIn({"key": "service.name", "value": {"stringValue": "my-gateway"}}, rs["resource"]["attributes"])
        span = rs["scopeSpans"][0]["spans"][0]
        self.assertEqual(span["traceId"], TRACE)
        self.assertEqual(span["parentSpanId"], "0011223344556677")
        self.assertEqual(span["kind"], 2)
        self.assertEqual(span["status"], {"code": 1})
        self.assertEqual(int(span["endTimeUnixNano"]) - int(span["startTimeUnixNano"]), 25000000)
        a = attrs(span)
        self.assertEqual(a["tool.name"], {"stringValue": "lookup_order"})
        self.assertEqual(a["latency_ms"], {"intValue": "25"})
        self.assertEqual(a["aws.operation.name"], {"stringValue": "CallTool"})

    def test_variants(self):
        rec = gateway_span(
            traceId="1-6a1f0c2b-9d3e4f5a6b7c8d9e0f1a2b3c",  # X-Ray format
            kind="SPAN_KIND_CLIENT",
            startTimeUnixNano="1731840000000000000",
            endTimeUnixNano="1731840000050000000",
            attributes=[{"key": "tool.name", "value": {"stringValue": "x"}}, {"key": "n", "value": {"intValue": "3"}}],
            status={"code": "STATUS_CODE_ERROR", "message": "target failed"},
        )
        span = h.to_otlp([rec])["resourceSpans"][0]["scopeSpans"][0]["spans"][0]
        self.assertEqual(span["traceId"], TRACE)
        self.assertEqual(span["kind"], 3)
        self.assertEqual(span["status"], {"code": 2, "message": "target failed"})
        self.assertEqual(span["endTimeUnixNano"], "1731840000050000000")
        self.assertEqual(attrs(span)["n"], {"intValue": "3"})

    def test_seconds_and_millis(self):
        self.assertEqual(h.nanos(1731840000.5), 1731840000500000000)
        self.assertEqual(h.nanos(1731840000500), 1731840000500000000)

    def test_bad_ids_dropped(self):
        self.assertEqual(h.to_otlp([gateway_span(traceId="nothex"), gateway_span(spanId="")]), {"resourceSpans": []})

    def test_groups_by_resource(self):
        other = gateway_span(spanId="ffffffffffffffff", resource={"attributes": {"service.name": "b"}})
        self.assertEqual(len(h.to_otlp([gateway_span(), other])["resourceSpans"]), 2)


class DecodeTest(unittest.TestCase):
    def test_filters_by_prefix_and_skips_junk(self):
        msgs = [json.dumps(gateway_span()), json.dumps(gateway_span(name="chat")), "not json", "[]"]
        recs = h.parse_spans(h.decode_event(subscription_event(msgs)), ("AgentCore.",))
        self.assertEqual([r["name"] for r in recs], ["AgentCore.Gateway.InvokeTool"])

    def test_control_message(self):
        self.assertEqual(h.decode_event(subscription_event(["x"], "CONTROL_MESSAGE")), [])

    def test_endpoint_requires_https(self):
        with self.assertRaises(h.SendError):
            h.endpoint_url({"BINDLE_ENDPOINT": "http://ingest.example.com"})
        self.assertEqual(h.endpoint_url({"BINDLE_ENDPOINT": "https://ingest.example.com/"}), "https://ingest.example.com/v1/traces")


class Ingest(BaseHTTPRequestHandler):
    responses = []
    received = []

    def log_message(self, *a):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        Ingest.received.append((self.path, dict(self.headers), json.loads(gzip.decompress(body))))
        code = Ingest.responses.pop(0) if Ingest.responses else 200
        self.send_response(code)
        if code == 429:
            self.send_header("Retry-After", "0")
        self.end_headers()


class ForwardTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = HTTPServer(("127.0.0.1", 0), Ingest)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.env = {
            "BINDLE_ENDPOINT": f"http://127.0.0.1:{cls.server.server_port}",
            "BINDLE_ALLOW_HTTP": "1",
            "BINDLE_API_KEY_SECRET_ARN": "arn:test",
        }

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def setUp(self):
        Ingest.responses, Ingest.received = [], []
        h._key_cache.clear()

    def test_forwards_with_key(self):
        out = h.forward(subscription_event([json.dumps(gateway_span())]), self.env, lambda arn: "bd_test_key\n", sleep=lambda s: None)
        self.assertEqual(out, {"forwarded": 1})
        path, headers, body = Ingest.received[0]
        self.assertEqual(path, "/v1/traces")
        self.assertEqual(headers["Authorization"], "Bearer bd_test_key")
        self.assertEqual(headers["Content-Encoding"], "gzip")
        self.assertEqual(body["resourceSpans"][0]["scopeSpans"][0]["spans"][0]["spanId"], SPAN)

    def test_retries_then_succeeds(self):
        Ingest.responses = [429, 503]
        out = h.forward(subscription_event([json.dumps(gateway_span())]), self.env, lambda arn: "k", sleep=lambda s: None)
        self.assertEqual(out, {"forwarded": 1})
        self.assertEqual(len(Ingest.received), 3)

    def test_gives_up_on_auth_error(self):
        Ingest.responses = [401]
        with self.assertRaises(h.SendError):
            h.forward(subscription_event([json.dumps(gateway_span())]), self.env, lambda arn: "k", sleep=lambda s: None)
        self.assertEqual(len(Ingest.received), 1)

    def test_nothing_to_send_skips_secret(self):
        def no_secret(arn):
            raise AssertionError("secret fetched")
        out = h.forward(subscription_event([json.dumps(gateway_span(name="chat"))]), self.env, no_secret)
        self.assertEqual(out, {"forwarded": 0})
        self.assertEqual(Ingest.received, [])


if __name__ == "__main__":
    unittest.main()
