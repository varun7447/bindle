// Exporter wrapper that redacts spans before they leave the process. Spans
// are read-only once ended, so each is copied with cleaned attributes.

import { ExportResultCode, type ExportResult } from '@opentelemetry/core';
import type { ReadableSpan, SpanExporter } from '@opentelemetry/sdk-trace';
import type { Redactor } from './redact.js';

export const CONTENT_KEYS = [
  'input.value',
  'output.value',
  'gen_ai.prompt',
  'gen_ai.completion',
  'bindle.input',
  'bindle.output',
  'bindle.reasoning',
  // OpenTelemetry GenAI conventions (Microsoft Agent Framework, Strands, ADK)
  'gen_ai.input.messages',
  'gen_ai.output.messages',
  'gen_ai.system_instructions',
  'gen_ai.tool.call.arguments',
  'gen_ai.tool.call.result',
] as const;

// When content is dropped, spans say who turned capture off, so the console
// can explain an empty input/output: "sdk" (this app's code or environment)
// or "workspace" (the console's SDK settings).
export const DROPPED_ATTR = 'bindle.content_dropped';
export type DroppedBy = 'sdk' | 'workspace';

export class RedactingSpanExporter implements SpanExporter {
  // One object, replaced atomically by update(), read once per export.
  private policy: { capture: boolean; redactor: Redactor; droppedBy: DroppedBy };

  constructor(
    private readonly delegate: SpanExporter,
    redactor: Redactor,
    capture: boolean,
    droppedBy: DroppedBy = 'sdk',
  ) {
    this.policy = { capture, redactor, droppedBy };
  }

  update(capture: boolean, redactor: Redactor, droppedBy: DroppedBy = 'sdk'): void {
    this.policy = { capture, redactor, droppedBy };
  }

  get captureContent(): boolean {
    return this.policy.capture;
  }

  clean(span: ReadableSpan, capture: boolean, redactor: Redactor, droppedBy: DroppedBy = 'sdk'): ReadableSpan {
    const attrs = { ...span.attributes };
    if (!capture) {
      for (const k of CONTENT_KEYS) delete attrs[k];
      attrs[DROPPED_ATTR] = droppedBy;
    }
    const events = span.events
      .filter((e) => capture || !e.name.startsWith('gen_ai.')) // GenAI events carry message content
      .map((e) => ({ ...e, attributes: redactor.applyAttributes(e.attributes) }));
    const status = span.status.message ? { ...span.status, message: redactor.apply(span.status.message) } : span.status;
    return {
      name: span.name,
      kind: span.kind,
      spanContext: () => span.spanContext(),
      parentSpanContext: span.parentSpanContext,
      startTime: span.startTime,
      endTime: span.endTime,
      status,
      attributes: redactor.applyAttributes(attrs),
      links: span.links,
      events,
      duration: span.duration,
      ended: span.ended,
      resource: span.resource,
      instrumentationScope: span.instrumentationScope,
      droppedAttributesCount: span.droppedAttributesCount,
      droppedEventsCount: span.droppedEventsCount,
      droppedLinksCount: span.droppedLinksCount,
    };
  }

  export(spans: ReadableSpan[], done: (r: ExportResult) => void): void {
    const { capture, redactor, droppedBy } = this.policy;
    let cleaned: ReadableSpan[];
    try {
      cleaned = spans.map((s) => this.clean(s, capture, redactor, droppedBy));
    } catch (e) {
      // Never send unredacted data on error.
      done({ code: ExportResultCode.FAILED, error: e as Error });
      return;
    }
    this.delegate.export(cleaned, done);
  }

  shutdown(): Promise<void> {
    return this.delegate.shutdown();
  }

  forceFlush(): Promise<void> {
    return this.delegate.forceFlush?.() ?? Promise.resolve();
  }
}
