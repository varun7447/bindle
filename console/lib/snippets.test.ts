import { describe, expect, it } from 'vitest';
import { ENDPOINT_PLACEHOLDER, envBlock, ingestUrl } from './snippets';

describe('ingestUrl', () => {
  it('uses the configured URL without a trailing slash', () => {
    expect(ingestUrl(' https://ingest.example.com/ ')).toBe('https://ingest.example.com');
  });
  it('shows a placeholder, never a built-in default, when unset', () => {
    expect(ingestUrl(undefined)).toBe(ENDPOINT_PLACEHOLDER);
    expect(ingestUrl('')).toBe(ENDPOINT_PLACEHOLDER);
    expect(envBlock(ingestUrl(undefined))).toContain('BINDLE_ENDPOINT=<your Bindle ingest URL>');
  });
});
