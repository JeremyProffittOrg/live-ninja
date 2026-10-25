import test from 'node:test';
import assert from 'node:assert/strict';
import { quotaExceededMessage } from '../../../web/static/js/mic.mjs';

test('monthly token limit does not claim typing still works', () => {
  const message = quotaExceededMessage({ kind: 'monthly_tokens', resetAt: '2026-11-01T00:00:00Z' });
  assert.match(message, /monthly usage limit/);
  assert.match(message, /typed messages/);
  assert.match(message, /It resets /);
  assert.doesNotMatch(message, /You can still type below/);
  assert.doesNotMatch(message, /soon/);
});

test('daily voice limit still offers typing', () => {
  const message = quotaExceededMessage({ kind: 'daily_minutes', resetAt: '2026-10-25T00:00:00Z' });
  assert.match(message, /today's voice limit/);
  assert.match(message, /You can still type below\./);
});
