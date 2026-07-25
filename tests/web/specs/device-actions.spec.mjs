import { test, expect } from '@playwright/test';
import { createDeferredDeviceActionGate } from '../../../web/static/js/deviceactions.mjs';

test('device session action waits for the spoken acknowledgement response', () => {
  const ready = [];
  const gate = createDeferredDeviceActionGate((action) => ready.push(action));

  gate.queue('stop_listening');
  gate.responseDone(); // function-calling response
  expect(ready).toEqual([]);

  gate.responseDone(); // acknowledgement response
  expect(ready).toEqual(['stop_listening']);

  gate.responseDone(); // no replay
  expect(ready).toEqual(['stop_listening']);
});

test('clearing the gate prevents a stale action crossing sessions', () => {
  const ready = [];
  const gate = createDeferredDeviceActionGate((action) => ready.push(action));

  gate.queue('start_new_conversation');
  gate.responseDone();
  gate.clear();
  gate.responseDone();

  expect(ready).toEqual([]);
});
