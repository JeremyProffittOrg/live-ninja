import { test } from 'node:test';
import assert from 'node:assert/strict';
import { ApiError, createToolDispatcher } from '../../../web/static/js/toolclient.mjs';

test('human-review proposal details reach the conversation without claiming execution', async () => {
  const details = { operation: 'save', memoryUrl: '/memory', proposed: { name: 'packing-rule', body: 'Private proposed rule' } };
  const events = [];
  const dispatcher = createToolDispatcher({
    sendEvent: event => events.push(event),
    invokeLocal: () => { throw new ApiError(400, { error: { code: 'confirmation_required', message: 'Review required.', details } }); },
  });
  await dispatcher.dispatch({ name: 'rule_save', callId: 'proposal-1', argsJson: '{}' });
  const output = JSON.parse(events.find(event => event.type === 'conversation.item.create').item.output);
  assert.equal(output.error, 'confirmation_required');
  assert.deepEqual(output.details, details);
  assert.equal(output.ok, undefined);
});

test('unrelated failure details are not copied into model output', async () => {
  const events = [];
  const dispatcher = createToolDispatcher({ sendEvent: event => events.push(event), invokeLocal: () => {
    throw new ApiError(500, { error: { code: 'upstream_error', message: 'Failed.', details: { diagnostic: 'private' } } });
  }});
  await dispatcher.dispatch({ name: 'other', callId: 'error-1', argsJson: '{}' });
  const output = JSON.parse(events.find(event => event.type === 'conversation.item.create').item.output);
  assert.equal(output.details, undefined);
});
