import { context } from '@opentelemetry/api';
import { suppressTracing } from '@opentelemetry/core';

// Remote settings from the Bindle console (sample rate, content capture,
// extra redaction patterns), polled from GET {endpoint}/v1/config. They are
// a ceiling: code can only be stricter. Never throws, never blocks the event
// loop from exiting (timers are unref'd), backs off when Bindle is down.

export interface RemoteSettings {
  version: number;
  sampleRate: number;
  captureContent: boolean;
  redactPatterns: Record<string, string>;
  pollIntervalSeconds: number;
}

const MIN_POLL = 30;
const MAX_POLL = 3600;
const MAX_BACKOFF = 600;
const MAX_BODY = 64 * 1024;
const NAME_RE = /^[A-Z][A-Z0-9_]{0,31}$/;

/** Parses and validates a /v1/config body; undefined if unusable. */
export function parseSettings(body: string): RemoteSettings | undefined {
  try {
    const d = JSON.parse(body) as Record<string, unknown>;
    const rate = Number(d.sample_rate);
    if (!Number.isFinite(rate) || rate < 0 || rate > 1 || typeof d.capture_content !== 'boolean') return undefined;
    const patterns: Record<string, string> = {};
    for (const p of Array.isArray(d.redact_patterns) ? d.redact_patterns : []) {
      const { name, pattern } = (p ?? {}) as { name?: unknown; pattern?: unknown };
      if (typeof name === 'string' && typeof pattern === 'string' && NAME_RE.test(name) && pattern.length > 0 && pattern.length <= 200) {
        patterns[name] = pattern;
      }
    }
    const poll = Number(d.poll_interval_seconds ?? 60);
    return {
      version: Number.isInteger(d.version) ? (d.version as number) : 0,
      sampleRate: rate,
      captureContent: d.capture_content,
      redactPatterns: patterns,
      pollIntervalSeconds: Math.min(Math.max(Number.isFinite(poll) ? poll : 60, MIN_POLL), MAX_POLL),
    };
  } catch {
    return undefined;
  }
}

export class Poller {
  private etag: string | undefined;
  private interval = 60;
  private failures = 0;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private stopped = false;

  constructor(
    private readonly url: string,
    private readonly apiKey: string,
    private readonly onUpdate: (s: RemoteSettings) => void,
    private readonly userAgent: string,
    private readonly fetchImpl: typeof fetch = fetch,
    private readonly log: (msg: string) => void = () => {},
  ) {}

  start(): void {
    void this.loop();
  }

  stop(): void {
    this.stopped = true;
    if (this.timer) clearTimeout(this.timer);
  }

  nextDelaySeconds(): number {
    const base = this.failures === 0 ? this.interval : Math.min(this.interval * 2 ** this.failures, MAX_BACKOFF);
    return base * (0.9 + Math.random() * 0.2); // jitter: fleets don't poll in lockstep
  }

  private async loop(): Promise<void> {
    if (this.stopped) return;
    await this.pollOnce();
    if (this.stopped) return;
    this.timer = setTimeout(() => void this.loop(), this.nextDelaySeconds() * 1000);
    this.timer.unref?.();
  }

  async pollOnce(): Promise<void> {
    try {
      const headers: Record<string, string> = { authorization: `Bearer ${this.apiKey}`, accept: 'application/json', 'user-agent': this.userAgent };
      if (this.etag) headers['if-none-match'] = this.etag;
      // Auto-instrumentation must not trace Bindle's own settings requests.
      const res = await context.with(suppressTracing(context.active()), () =>
        this.fetchImpl(this.url, { headers, signal: AbortSignal.timeout(5000) }),
      );
      if (res.status === 304) {
        this.failures = 0;
        return;
      }
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const body = await res.text();
      if (body.length > MAX_BODY) throw new Error('config response too large');
      const s = parseSettings(body);
      if (!s) throw new Error('unusable config response');
      this.etag = res.headers.get('etag') ?? undefined;
      this.interval = s.pollIntervalSeconds;
      this.failures = 0;
      this.onUpdate(s);
    } catch (e) {
      this.failures = Math.min(this.failures + 1, 10);
      this.log(`bindle: remote config unavailable: ${(e as Error).message}`);
    }
  }
}
