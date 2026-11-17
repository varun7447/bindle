// Head sampler with a rate that remote config can change at runtime.
//
// Uses the same rule as the Python SDK and Bindle's ingest: a trace is kept
// when the lower 64 bits of its ID are below round(rate * 2^64). OpenTelemetry
// JS's TraceIdRatioBasedSampler uses a different hash, so it would keep a
// different set of traces than ingest; shared test vectors pin this one.

import type { Attributes, Context, Link, SpanKind } from '@opentelemetry/api';
import { SamplingDecision, type Sampler, type SamplingResult } from '@opentelemetry/sdk-trace';

const TWO_64 = 2 ** 64;

export function boundForRate(rate: number): bigint {
  return BigInt(Math.round(Math.min(Math.max(rate, 0), 1) * TWO_64));
}

export function sampled(traceId: string, rate: number): boolean {
  if (!/^[0-9a-f]{32}$/i.test(traceId)) return false;
  return BigInt('0x' + traceId.slice(16)) < boundForRate(rate);
}

export class DynamicRatioSampler implements Sampler {
  private bound: bigint;
  private _rate: number;

  constructor(rate: number) {
    this._rate = Math.min(Math.max(rate, 0), 1);
    this.bound = boundForRate(this._rate);
  }

  get rate(): number {
    return this._rate;
  }

  setRate(rate: number): void {
    this._rate = Math.min(Math.max(rate, 0), 1);
    this.bound = boundForRate(this._rate);
  }

  shouldSample(_c: Context, traceId: string, _n: string, _k: SpanKind, _a: Attributes, _l: Link[]): SamplingResult {
    const keep = /^[0-9a-f]{32}$/i.test(traceId) && BigInt('0x' + traceId.slice(16)) < this.bound;
    return { decision: keep ? SamplingDecision.RECORD_AND_SAMPLED : SamplingDecision.NOT_RECORD };
  }

  toString(): string {
    return `BindleDynamicRatio{${this._rate}}`;
  }
}
