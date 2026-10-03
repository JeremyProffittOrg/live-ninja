import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildSchedule, localTimeToISO, inTimezone, scheduleLabel, statusLabel, createRequestKeys, errorMessage } from '../../../web/static/js/jobs.mjs';

test('once schedule resolves the chosen zone rather than the computer zone', () => {
  const schedule = buildSchedule({ kind: 'once', timezone: 'America/New_York', at: '2030-07-04T09:15' }, 0);
  assert.equal(schedule.at, '2030-07-04T13:15:00.000Z');
  assert.equal(inTimezone(schedule.at, schedule.timezone), '2030-07-04T09:15');
  assert.equal(localTimeToISO('2030-01-04T09:15', 'America/New_York'), '2030-01-04T14:15:00.000Z');
});

test('one-time DST gaps and overlaps cannot silently move a commitment', () => {
  assert.throws(() => localTimeToISO('2030-03-10T02:30', 'America/New_York'), /does not exist/);
  assert.throws(() => localTimeToISO('2030-11-03T01:30', 'America/New_York'), /occurs twice/);
  assert.equal(localTimeToISO('2030-11-03T06:30', 'UTC'), '2030-11-03T06:30:00.000Z');
});

test('fractional timezone offsets and midnight round trip accurately', () => {
  assert.equal(localTimeToISO('2030-07-04T00:00', 'Asia/Kathmandu'), '2030-07-03T18:15:00.000Z');
  assert.equal(inTimezone('2030-07-03T18:15:00Z', 'Asia/Kathmandu'), '2030-07-04T00:00');
});

test('invalid dates, stale dates, missing zones and malformed recurrence fail before mutation', () => {
  assert.throws(() => localTimeToISO('2030-02-30T09:00', 'UTC'), /valid date/);
  assert.throws(() => buildSchedule({ kind: 'once', timezone: 'UTC', at: '2000-01-01T00:00' }), /future/);
  assert.throws(() => buildSchedule({ kind: 'manual', timezone: '' }), /timezone/);
  assert.throws(() => buildSchedule({ kind: 'daily', timezone: 'Mars', time: '09:00' }), /timezone/);
  assert.throws(() => buildSchedule({ kind: 'daily', timezone: 'UTC', time: '24:00' }), /valid time/);
  assert.throws(() => buildSchedule({ kind: 'weekly', timezone: 'UTC', time: '09:00', weekday: 7 }), /day of the week/);
});

test('manual and recurrence contracts keep timezone and weekday without inventing a next run', () => {
  assert.deepEqual(buildSchedule({ kind: 'manual', timezone: 'UTC' }), { kind: 'once', timezone: 'UTC' });
  assert.deepEqual(buildSchedule({ kind: 'weekdays', timezone: 'Europe/London', time: '08:30' }), { kind: 'weekdays', timezone: 'Europe/London', time: '08:30' });
  const schedule = buildSchedule({ kind: 'weekly', timezone: 'America/Chicago', time: '09:00', weekday: '0' });
  assert.deepEqual(schedule, { kind: 'weekly', timezone: 'America/Chicago', time: '09:00', weekday: 0 });
  assert.match(scheduleLabel(schedule), /Every Sunday at 09:00/);
  assert.equal(scheduleLabel({ kind: 'once', timezone: 'UTC' }), 'Manual only');
});

test('unknown outcomes reuse the request ID; changed payloads and confirmed new actions do not', () => {
  let serial = 0; const keys = createRequestKeys(() => `request-${++serial}`);
  const payload = { expectedVersion: 3 };
  const id = keys.get('/jobs/1/run', payload);
  assert.equal(keys.get('/jobs/1/run', { expectedVersion: 3 }), id);
  assert.notEqual(keys.get('/jobs/1/run', { expectedVersion: 4 }), id);
  assert.notEqual(keys.get('/jobs/1/cancel', payload), id);
  keys.clear('/jobs/1/run', payload);
  assert.notEqual(keys.get('/jobs/1/run', payload), id);
});

test('queued and review states never imply success; stale errors give a recovery action', () => {
  assert.equal(statusLabel('queued'), 'Queued');
  assert.equal(statusLabel('waiting_approval'), 'Needs review');
  assert.match(errorMessage({ status: 409 }), /latest version/);
  assert.match(errorMessage({ status: 503 }), /input is kept/);
});
