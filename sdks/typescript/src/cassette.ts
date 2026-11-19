import { readFileSync } from 'node:fs';
import { canonicalArgs } from './limits.js';

// Replay a recorded run with its tool outputs. Download a run from the
// Bindle console (Run details → Download for replay), re-run your agent on
// cassette.input, and wrap each tool with cassette.wrap(name, fn) so calls
// are answered from the recording instead of the real tool.
//
// Calls are matched by tool name and arguments (key order ignored);
// repeated identical calls get the recorded results in order. A call with
// new arguments gets the next unused recorded result for that tool (set
// matchOrder: false to turn this off). A call the run never made throws
// UnrecordedCallError, unless onMissing is 'call' (then the real tool runs).
// A tool call that failed in the recording throws RecordedToolError.

export const CASSETTE_FORMAT = 'bindle.cassette';

export class UnrecordedCallError extends Error {
  constructor(
    readonly tool: string,
    readonly args: string,
  ) {
    super(`the recorded run never called ${tool}${args ? ` (arguments ${args.slice(0, 200)})` : ''}; use onMissing: 'call' to call the real tool`);
    this.name = 'UnrecordedCallError';
  }
}

export class RecordedToolError extends Error {
  constructor(
    readonly tool: string,
    message: string,
  ) {
    super(message || `${tool} failed in the recorded run`);
    this.name = 'RecordedToolError';
  }
}

export interface RecordedCall {
  index: number;
  tool: string;
  arguments: string; // canonical
  result: string;
  status: string;
  error: string;
  stepId: string;
}

export interface CassetteOptions {
  onMissing?: 'error' | 'call';
  matchOrder?: boolean;
}

export interface CassetteReport {
  traceId: string;
  recordedCalls: number;
  matchedByArguments: number;
  matchedByOrder: number;
  missing: string[];
  unused: string[];
  incomplete: string[];
}

const decode = (t: string): unknown => {
  try {
    return JSON.parse(t);
  } catch {
    return t;
  }
};

export class Cassette {
  readonly traceId: string;
  readonly agent: string;
  readonly agentVersion: string;
  readonly input: string;
  readonly output: string;
  readonly incomplete: string[];
  readonly calls: RecordedCall[];
  private readonly onMissing: 'error' | 'call';
  private readonly matchOrder: boolean;
  private readonly used = new Set<number>();
  private log: { tool: string; match: 'arguments' | 'order' | 'missing' }[] = [];

  constructor(data: unknown, opts: CassetteOptions = {}) {
    const d = (data ?? {}) as Record<string, unknown>;
    if (d.format !== CASSETTE_FORMAT) throw new Error('not a Bindle cassette (download one from Run details → Download for replay)');
    if (Number(d.version ?? 0) > 1) throw new Error(`cassette version ${String(d.version)} is newer than this SDK; upgrade @bindle-ai/sdk`);
    this.traceId = String(d.trace_id ?? '');
    this.agent = String(d.agent ?? '');
    this.agentVersion = String(d.agent_version ?? '');
    this.input = String(d.input ?? '');
    this.output = String(d.output ?? '');
    this.incomplete = Array.isArray(d.incomplete) ? d.incomplete.map(String) : [];
    this.onMissing = opts.onMissing ?? 'error';
    this.matchOrder = opts.matchOrder ?? true;
    const calls: RecordedCall[] = [];
    (Array.isArray(d.tool_calls) ? d.tool_calls : []).forEach((c: unknown, i: number) => {
      const r = (c ?? {}) as Record<string, unknown>;
      if (!r.tool) return;
      calls.push({
        index: i,
        tool: String(r.tool),
        arguments: canonicalArgs(r.arguments ?? ''),
        result: String(r.result ?? ''),
        status: String(r.status ?? ''),
        error: String(r.error ?? ''),
        stepId: String(r.step_id ?? ''),
      });
    });
    this.calls = calls;
  }

  /** Reads a cassette downloaded from the console. */
  static load(path: string, opts: CassetteOptions = {}): Cassette {
    return new Cassette(JSON.parse(readFileSync(path, 'utf8')), opts);
  }

  private take(tool: string, keys: string[]): RecordedCall | undefined {
    for (const k of keys) {
      const c = this.calls.find((x) => !this.used.has(x.index) && x.tool === tool && x.arguments === k);
      if (c) {
        this.used.add(c.index);
        this.log.push({ tool, match: 'arguments' });
        return c;
      }
    }
    if (this.matchOrder) {
      const c = this.calls.find((x) => !this.used.has(x.index) && x.tool === tool);
      if (c) {
        this.used.add(c.index);
        this.log.push({ tool, match: 'order' });
        return c;
      }
    }
    this.log.push({ tool, match: 'missing' });
    return undefined;
  }

  private answer(c: RecordedCall, raw: boolean): unknown {
    if (c.status === 'failure') throw new RecordedToolError(c.tool, c.error);
    return raw ? c.result : decode(c.result);
  }

  /** The recorded result of calling tool with args (JSON results decoded unless raw). */
  result(tool: string, args: unknown = '', opts: { raw?: boolean } = {}): unknown {
    const key = canonicalArgs(args);
    const c = this.take(tool, [key]);
    if (!c) throw new UnrecordedCallError(tool, key);
    return this.answer(c, opts.raw ?? false);
  }

  /**
   * Wraps a tool so calls are answered from the recording. A single object
   * argument is matched as the tool's arguments; otherwise the argument list.
   */
  wrap<A extends unknown[], R>(tool: string, fn: (...args: A) => R, opts: { raw?: boolean } = {}): (...args: A) => R {
    // Keep the tool's shape: async tools resolve or reject, sync tools return or throw.
    const isAsync = fn.constructor.name === 'AsyncFunction';
    return (...args: A): R => {
      const keys = args.length === 1 ? [canonicalArgs(args[0]), canonicalArgs(args)] : [canonicalArgs(args)];
      const c = this.take(tool, keys);
      if (!c && this.onMissing === 'call') return fn(...args);
      try {
        if (!c) throw new UnrecordedCallError(tool, keys[0]!);
        const v = this.answer(c, opts.raw ?? false);
        return (isAsync ? Promise.resolve(v) : v) as R;
      } catch (e) {
        if (isAsync) return Promise.reject(e) as R;
        throw e;
      }
    };
  }

  /** Recorded calls the replay never made (the new run took another path). */
  unused(): RecordedCall[] {
    return this.calls.filter((c) => !this.used.has(c.index));
  }

  report(): CassetteReport {
    return {
      traceId: this.traceId,
      recordedCalls: this.calls.length,
      matchedByArguments: this.log.filter((e) => e.match === 'arguments').length,
      matchedByOrder: this.log.filter((e) => e.match === 'order').length,
      missing: this.log.filter((e) => e.match === 'missing').map((e) => e.tool),
      unused: this.unused().map((c) => c.tool),
      incomplete: [...this.incomplete],
    };
  }

  /** Forget which recorded calls were used, to replay again. */
  reset(): void {
    this.used.clear();
    this.log = [];
  }
}
