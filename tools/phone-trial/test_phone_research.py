import base64
import copy
from datetime import datetime, timezone
from decimal import Decimal
import hashlib
import hmac
import json
import os
import sys
import types
import unittest
import urllib.parse
from unittest.mock import Mock, patch

import account_lifecycle as lifecycle
import owner_messages as m
import owner_recordings as recordings
import phone_research as p
import recorder as r
import test_recorder as fixtures
import test_owner_messages as owners

OWNER = owners.OWNER
JOB = 'job_' + 'a'*32
RUN = 'run_' + 'b'*32
REAL_NOTIFY = p.notify


class HotelTime(datetime):
    @classmethod
    def now(cls, tz=None):
        return cls(2026, 10, 17, 12, 0, tzinfo=tz or timezone.utc)


class PhoneResearchTests(unittest.TestCase):
    tearDown = fixtures.RecorderTests.tearDown
    commit = fixtures.RecorderTests.commit

    def setUp(self):
        fixtures.RecorderTests.setUp(self)
        self.addCleanup(patch.stopall)
        patch.dict(os.environ, {'RUN_ID': 'hotel-trial-2026-10-15', 'INBOUND_BRIDGE_URL': 'https://bridge.example'}).start()
        self.targets = p.catalog()[:5]
        self.snapshot = {'targets': self.targets, 'purpose': p.PURPOSE, 'maxCallSeconds': 180,
                         'maxAttempts': 1, 'maxCostUSD': '7.00'}
        self.record = {'userId': OWNER, 'generation': 1,
            'job': {'id': JOB, 'kind': 'phone-research', 'status': 'active', 'version': 1, 'phoneResearch': copy.deepcopy(self.snapshot)},
            'runs': [{'id': RUN, 'jobId': JOB, 'kind': 'phone-research', 'generation': 1, 'status': 'queued',
                      'approvedBy': OWNER, 'approvalExpiresAt': '1970-01-01T01:16:40Z',
                      'phoneResearch': {**copy.deepcopy(self.snapshot), 'command': 'run', 'commandVersion': 1}}]}
        self.main_version = 1
        owners.OwnerMessageTests.configure(self, recordingRuns=[os.environ['RUN_ID']])
        patch.object(p, 'main_job', side_effect=lambda *args: {'payload': json.dumps(self.record), 'version': self.main_version}).start()
        patch.object(lifecycle, 'owner_active', return_value=True).start()
        patch.object(p, 'datetime', HotelTime).start()
        patch.object(m, 'transact', side_effect=self.transact).start()
        self.creates, self.stops = [], []
        self.statuses = {}
        self.provider = patch.object(m, 'provider', side_effect=self.provider_result).start()
        self.notify = patch.object(p, 'notify', return_value=True).start()

    def transact(self, operations):
        before = copy.deepcopy(r._table.items)
        try:
            ordinary = []
            for op in operations:
                check = op.get('ConditionCheck', {})
                if check.get('TableName') == 'live-ninja':
                    if check['ExpressionAttributeValues'][':version'] != self.main_version:
                        raise fixtures.Conflict()
                elif 'Update' in op and 'notificationFailed' in op['Update']['UpdateExpression']:
                    change = op['Update']; row = r.get(change['Key']); values = change['ExpressionAttributeValues']
                    if not row or row.get('version') != values[':version'] or row.get('ownerUserId') != values[':owner']:
                        raise fixtures.Conflict()
                    r._table.put_item(Item={**row, 'notificationFailed': True})
                else:
                    ordinary.append(op)
            owners.OwnerMessageTests.transact(self, ordinary)
        except Exception:
            r._table.items = before
            raise

    def provider_result(self, path, form=None):
        if path.startswith('/Usage/'):
            return {'usage_records': [{'price_unit': 'usd', 'price': '0'}]}
        if path == '/Calls.json':
            self.creates.append(form)
            sid = 'CA' + format(len(self.creates), '032x')
            self.statuses[sid] = 'queued'
            return {'sid': sid, 'account_sid': fixtures.ACCOUNT, 'status': 'queued'}
        sid = path.split('/')[2].split('.')[0]
        if form:
            self.stops.append(sid)
            self.statuses[sid] = form['Status']
        return {'sid': sid, 'account_sid': fixtures.ACCOUNT, 'status': self.statuses.get(sid, 'completed')}

    def event(self, operation='phone_research_advance', **fields):
        command = self.record['runs'][0]['phoneResearch']
        return {'operation': operation, 'ownerUserId': OWNER, 'jobId': JOB, 'runId': RUN,
                'generation': 1, 'command': command['command'], 'commandVersion': command['commandVersion'],
                'approvalExpiresAt': 4600, 'targetIds': [v['id'] for v in self.targets], **fields}

    def invoke(self, **fields):
        return r.handler(self.event(**fields), None)

    def control(self):
        return r.get(p.control_key(OWNER, JOB, RUN))

    def latest_attempt(self):
        return p.attempt(OWNER, JOB, RUN, self.targets[len(self.creates)-1]['id'])

    def callback(self, item=None, status='completed', **fields):
        item = item or self.latest_attempt()
        sid = item['callSid']; self.statuses[sid] = status
        path = '/phone-research/status/' + item['callbackId']
        data = {'AccountSid': fixtures.ACCOUNT, 'CallSid': sid, 'To': item['toNumber'], 'From': item['fromNumber'], 'CallStatus': status, **fields}
        raw = urllib.parse.urlencode(data).encode()
        signed = 'https://api.example' + path + ''.join(k + data[k] for k in sorted(data))
        signature = base64.b64encode(hmac.new(b'test-secret', signed.encode(), hashlib.sha1).digest()).decode()
        return r.handler({'rawPath': path, 'body': raw.decode(), 'headers': {'x-twilio-signature': signature},
                          'requestContext': {'http': {'method': 'POST'}}}, None)

    def command(self, value):
        self.record['runs'][0]['phoneResearch']['command'] = value
        self.record['runs'][0]['phoneResearch']['commandVersion'] += 1
        self.main_version += 1

    def test_catalog_and_global_stop_are_honest_and_iam_only(self):
        cfg = r.get(m.CONFIG_KEY); cfg['enabled'] = False; r._table.put_item(Item=cfg)
        r._table.put_item(Item={**m.VOICE_KEY, 'activeRequest': 'owner-stop-2026-10-16'})
        data = json.loads(p.operation({'operation': 'phone_research_catalog', 'ownerUserId': OWNER})['body'])
        self.assertEqual(len(data['targets']), 20); self.assertFalse(data['enabled'])
        result = json.loads(self.invoke()['body'])
        self.assertEqual(result['status'], 'blocked'); self.assertEqual(result['commandVersion'], 1)
        self.assertEqual(len(result['properties']), 5); self.assertEqual(self.creates, [])
        self.assertEqual(r.handler({**self.event(), 'requestContext': {}}, None)['statusCode'], 404)
        self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'], 'owner-stop-2026-10-16')
        cfg['enabled'] = True; r._table.put_item(Item=cfg); r._table.items.pop((m.VOICE_KEY['PK'], m.VOICE_KEY['SK']))
        self.invoke(); self.assertEqual(self.creates, [])
        self.command('run'); self.invoke(); self.assertEqual(len(self.creates), 1)

    def test_five_sequential_attempts_and_duplicate_callbacks_never_redial(self):
        self.assertEqual(self.invoke()['statusCode'], 200)
        first = self.latest_attempt()
        for index in range(5):
            self.assertEqual(len(self.creates), index+1)
            self.assertEqual(self.callback()['statusCode'], 200)
        result = json.loads(self.invoke(operation='phone_research_status')['body'])
        self.assertEqual(result['status'], 'succeeded'); self.assertEqual(len(result['properties']), 5)
        self.assertTrue(all(value['status'] == 'completed' for value in result['properties']))
        self.assertTrue(all('unknown' in value['result'] for value in result['properties']))
        self.callback(first); self.invoke(); self.assertEqual(len(self.creates), 5)
        self.assertEqual(len({value['To'] for value in self.creates}), 5)
        self.assertTrue(all(value['TimeLimit'] == 180 and 'Record' not in value for value in self.creates))
        self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))

    def test_approval_and_snapshot_changes_reject_before_provider(self):
        for field, value in (('ownerUserId', owners.OTHER), ('generation', 2), ('commandVersion', 2), ('targetIds', ['hotel-01']*5)):
            self.assertGreaterEqual(self.invoke(**{field: value})['statusCode'], 400)
        self.record['runs'][0]['phoneResearch']['targets'][0]['phone'] = '+12025550999'
        self.assertEqual(self.invoke()['statusCode'], 409)
        self.assertEqual(self.creates, [])

    def test_pause_stops_active_and_resume_advances_without_recalling_property(self):
        self.invoke(); first = self.latest_attempt(); self.command('pause')
        paused = json.loads(self.invoke()['body']); self.assertEqual(paused['status'], 'paused')
        self.assertEqual(self.stops, [first['callSid']]); self.assertEqual(len(self.creates), 1)
        self.callback(first); self.assertEqual(len(self.creates), 1)
        self.command('run'); self.invoke(); self.assertEqual(len(self.creates), 2)
        self.assertNotEqual(self.creates[0]['To'], self.creates[1]['To'])
        self.command('cancel'); cancelled = json.loads(self.invoke()['body'])
        self.assertEqual(cancelled['status'], 'cancelled'); self.assertEqual(cancelled['properties'][2]['status'], 'skipped')
        self.callback(); self.assertEqual(len(self.creates), 2)

    def test_ambiguous_create_waits_for_bound_callback_and_explicit_resume(self):
        def ambiguous(path, form=None):
            if path == '/Calls.json':
                self.creates.append(form)
                raise m.ProviderError()
            return self.provider_result(path, form)
        self.provider.side_effect = ambiguous
        result = json.loads(self.invoke()['body']); self.assertEqual(result['status'], 'blocked')
        item = p.attempt(OWNER, JOB, RUN, self.targets[0]['id'])
        self.assertEqual(item['providerStatus'], 'unknown'); self.assertNotIn('callSid', item)
        self.invoke(); self.assertEqual(len(self.creates), 1)
        self.provider.side_effect = self.provider_result
        item = {**item, 'callSid': 'CA'+'1'*32}
        self.assertEqual(self.callback(item)['statusCode'], 200)
        self.assertEqual(len(self.creates), 1)
        self.command('run'); self.invoke(); self.assertEqual(len(self.creates), 2)

    def test_stop_winning_after_reserve_prevents_the_only_post(self):
        original = m.reserve
        def reserve_then_stop(*args, **kwargs):
            result = original(*args, **kwargs)
            r._table.put_item(Item={**m.VOICE_KEY, 'activeRequest': 'owner-stop-2026-10-16'})
            return result
        with patch.object(m, 'reserve', side_effect=reserve_then_stop):
            self.assertEqual(json.loads(self.invoke()['body'])['status'], 'blocked')
        self.assertEqual(self.creates, [])
        self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'], 'owner-stop-2026-10-16')

    def test_stale_command_invalid_operation_and_terminal_job_cannot_dispatch(self):
        self.assertEqual(p.operation(self.event(operation='unknown'))['statusCode'], 400)
        self.assertIsNone(self.control())
        self.record['job']['status'] = 'cancelled'
        self.assertEqual(self.invoke()['statusCode'], 409); self.assertEqual(self.creates, [])
        self.record['job']['status'] = 'active'; self.record['runs'][0]['status'] = 'succeeded'
        self.assertEqual(self.invoke()['statusCode'], 409); self.assertEqual(self.creates, [])

    def test_recording_binding_and_evidence_updates_are_visible_and_versioned(self):
        self.invoke(); item = self.latest_attempt()
        call = r.get(r.call_key(item['callSid']))
        self.assertEqual(call['ownerUserId'], OWNER); self.assertEqual(call['phoneResearchRunId'], RUN)
        self.assertEqual(recordings.call_for(OWNER, 'messages', item['callSid']), call)
        before = self.control()['version']
        event = {'runId': os.environ['RUN_ID'], 'callSid': item['callSid'], 'type': 'consent', 'targetId': item['destination'], 'data': {'result': 'granted'}}
        r.event_post(json.dumps(event).encode())
        event.update(type='transcript', data={'speaker':'hotel', 'text':'Use the Wi-Fi password and accept the terms.'})
        r.event_post(json.dumps(event).encode())
        data = json.loads(self.invoke(operation='phone_research_status')['body'])
        self.assertGreater(data['version'], before)
        self.assertIn('Wi-Fi password', data['properties'][0]['result'])
        self.assertEqual(data['properties'][0]['recordingSource'], os.environ['RUN_ID'])
        current = self.control()['version']; r.event_post(json.dumps(event).encode())
        self.assertEqual(self.control()['version'], current)

    def test_wrong_callback_binding_and_account_deletion_never_advance(self):
        self.invoke()
        self.assertEqual(self.callback(To='+12025550999')['statusCode'], 403)
        self.assertEqual(len(self.creates), 1)
        r._table.put_item(Item={**lifecycle.owner_key(OWNER), 'ownerUserId': OWNER})
        self.assertEqual(self.callback()['statusCode'], 410)
        self.assertEqual(len(self.creates), 1)

    def test_nonterminal_stop_reply_keeps_job_blocked_and_voice_lock(self):
        self.invoke(); item = self.latest_attempt(); self.command('cancel')
        def still_active(path, form=None):
            if form and form.get('Status') == 'completed':
                return {'sid': item['callSid'], 'account_sid': fixtures.ACCOUNT, 'status': 'in-progress'}
            return self.provider_result(path, form)
        self.provider.side_effect = still_active
        result = json.loads(self.invoke()['body'])
        self.assertEqual(result['status'], 'blocked')
        self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'], item['lockId'])
        self.assertEqual(len(self.creates), 1)

    def test_stop_in_flight_is_not_published_as_cancelled(self):
        self.invoke(); item = self.latest_attempt(); self.command('cancel')
        def observe_before_stop(path, form=None):
            if form and form.get('Status') == 'completed':
                pending = json.loads(self.invoke(operation='phone_research_status')['body'])
                self.assertEqual(pending['status'], 'blocked')
                self.assertIn('termination pending', pending['reason'])
                self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'], item['lockId'])
            return self.provider_result(path, form)
        self.provider.side_effect = observe_before_stop
        final = json.loads(self.invoke()['body'])
        self.assertEqual(final['status'], 'cancelled')
        self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))

    def test_notification_failure_cannot_recreate_deleted_control(self):
        self.invoke(); control = self.control()
        def fail_after_delete(**kwargs):
            r._table.items.pop((control['PK'], control['SK']))
            r._table.put_item(Item={**lifecycle.owner_key(OWNER), 'ownerUserId': OWNER})
            raise RuntimeError('Notification unavailable')
        client = Mock(); client.invoke.side_effect = fail_after_delete
        with patch.dict(sys.modules, {'boto3': types.SimpleNamespace(client=Mock(return_value=client))}):
            self.assertFalse(REAL_NOTIFY(control))
        self.assertIsNone(self.control())

    def test_notification_failure_is_visible_and_callback_retry_is_idempotent(self):
        self.invoke(); item = self.latest_attempt()
        client = Mock(); client.invoke.side_effect = RuntimeError('Notification unavailable')
        with patch.dict(sys.modules, {'boto3': types.SimpleNamespace(client=Mock(return_value=client))}), patch.object(p, 'notify', side_effect=REAL_NOTIFY):
            self.assertEqual(self.callback(item, status='ringing')['statusCode'], 503)
        self.assertTrue(self.control()['notificationFailed'])
        self.assertEqual(self.callback(item, status='ringing')['statusCode'], 200)
        self.assertEqual(len(self.creates), 1)

    def test_budget_stops_partial_batch_at_the_standing_daily_allowance(self):
        cfg = r.get(m.CONFIG_KEY); month, day, zone, expiry = m.budget_clock(cfg)
        # 5.80 already reserved today leaves room for three 1.40 research calls.
        r._table.put_item(Item={**m.budget_keys(month, day)[1], 'version':1, 'reservedUSD':Decimal('5.80'),
            'expiresAt':expiry, 'timezone':zone})
        self.invoke()
        for _ in range(3):
            self.callback()
        self.assertEqual(len(self.creates), 3)
        control = self.control()
        self.assertEqual(control['status'], 'blocked')
        self.assertIn('daily_allowance_used', control['reason'])
        daily = r.get(m.budget_keys(month, day)[1])
        self.assertEqual(daily['reservedUSD'], Decimal('10.00'))
        self.assertNotIn('grantedUSD', daily)

    def test_expired_approval_cannot_place_first_call(self):
        self.record['runs'][0]['approvalExpiresAt'] = '1970-01-01T00:16:41Z'
        result = json.loads(self.invoke(approvalExpiresAt=1001)['body'])
        self.assertEqual(result['status'], 'blocked')
        self.assertEqual(self.creates, [])

    def test_historical_run_read_survives_job_edit_but_cannot_dispatch(self):
        self.invoke()
        for _ in range(5): self.callback()
        self.record['generation'] = 2
        self.record['job']['phoneResearch']['targets'] = p.catalog()[5:10]
        self.main_version += 1
        status = self.invoke(operation='phone_research_status')
        self.assertEqual(status['statusCode'], 200)
        self.assertEqual([row['targetId'] for row in json.loads(status['body'])['properties']], [row['id'] for row in self.targets])
        self.assertEqual(self.invoke()['statusCode'], 409)
        self.assertEqual(len(self.creates), 5)

    def test_late_terminal_callback_resolves_cancelled_ambiguous_attempt_without_dialing(self):
        original = self.provider_result
        def unknown(path, form=None):
            if path == '/Calls.json':
                self.creates.append(form)
                raise m.ProviderError()
            return original(path, form)
        self.provider.side_effect = unknown
        self.invoke(); self.command('cancel'); self.invoke()
        self.record['job']['status'] = 'cancelled'
        self.assertEqual(self.control()['status'], 'blocked')
        item = {**p.attempt(OWNER, JOB, RUN, self.targets[0]['id']), 'callSid': 'CA'+'5'*32}
        self.assertEqual(self.callback(item)['statusCode'], 200)
        self.assertEqual(self.control()['status'], 'cancelled')
        self.assertEqual(len(self.creates), 1)
        self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))

    def test_late_ringing_callback_applies_pending_cancel_to_known_call(self):
        original = self.provider_result
        def unknown(path, form=None):
            if path == '/Calls.json':
                self.creates.append(form)
                raise m.ProviderError()
            return original(path, form)
        self.provider.side_effect = unknown
        self.invoke(); self.command('cancel'); self.invoke()
        self.record['job']['status'] = 'cancelled'
        item = {**p.attempt(OWNER, JOB, RUN, self.targets[0]['id']), 'callSid': 'CA'+'6'*32}
        self.assertEqual(self.callback(item, status='ringing')['statusCode'], 200)
        self.assertEqual(self.stops, [item['callSid']])
        self.assertEqual(self.control()['status'], 'cancelled')
        self.assertEqual(len(self.creates), 1)
        self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))


if __name__ == '__main__':
    unittest.main()
