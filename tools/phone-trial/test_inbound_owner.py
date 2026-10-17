import base64
import hashlib
import hmac
import json
import importlib.util
import io
import os
from pathlib import Path
import sys
import types
from decimal import Decimal
import unittest
import urllib.parse
from unittest.mock import patch, Mock

import inbound_owner as i
import owner_messages as m
import recorder as r
import test_recorder as f
import test_owner_messages as owners

REAL_MINT = i.mint


class InboundTests(unittest.TestCase):
    setUp = f.RecorderTests.setUp
    tearDown = f.RecorderTests.tearDown
    commit = f.RecorderTests.commit

    def configure(self):
        owners.OwnerMessageTests.configure(self)
        salt = '1' * 32
        r._table.put_item(Item={**i.CONFIG, 'version': 1, 'enabled': True, 'ownerUserId': owners.OWNER,
            'pinSalt': salt, 'pinHash': hashlib.pbkdf2_hmac('sha256', b'123456', bytes.fromhex(salt), i.PIN_ITERATIONS).hex(),
            'pinIterations': i.PIN_ITERATIONS})
        self.addCleanup(patch.stopall)
        patch.dict(os.environ, {'INBOUND_BRIDGE_URL': 'https://bridge.example'}).start()
        self.owner_active = patch('account_lifecycle.owner_active', return_value=True).start()
        patch.object(m, 'transact', self.transact).start()
        self.provider = patch.object(m, 'provider', side_effect=self.provider_result).start()
        self.broker = patch.object(i, 'mint', return_value={'mode': 'phone', 'bridgeToken': 'test-capability',
            'sessionId': 'fresh-session', 'sessionConfig': {'instructions': 'owner context', 'tools': []}}).start()
        self.final = patch.object(i, 'final_transcript').start()

    def transact(self, operations):
        regular = []
        for operation in operations:
            check = operation.get('ConditionCheck', {})
            if check.get('Key') == i.CONFIG:
                cfg = r.get(i.CONFIG) or {}
                values = check['ExpressionAttributeValues']
                if cfg.get('enabled') is not True or any(cfg.get(field) != values[name] for field, name in
                    (('ownerUserId', ':owner'), ('pinHash', ':hash'), ('pinSalt', ':salt'))):
                    raise f.Conflict()
            else:
                regular.append(operation)
        owners.OwnerMessageTests.transact(self, regular)

    def provider_result(self, path, form=None):
        if path.startswith('/Usage/'):
            return {'usage_records': [{'price_unit': 'usd', 'price': '0'}]}
        if path.endswith('/Recordings.json'):
            return {'sid': f.REC, 'call_sid': f.CALL, 'account_sid': f.ACCOUNT}
        return {'sid': f.CALL, 'account_sid': f.ACCOUNT, 'status': 'completed' if (form or {}).get('Status') == 'completed' else 'in-progress'}

    def item(self):
        return r.get(m.request_key(owners.OWNER, 'inbound-' + f.CALL))

    def callback(self, kind='voice', **fields):
        path = '/inbound-owner/' + kind
        if kind != 'voice':
            path += '/' + self.item()['callbackId']
        data = {'AccountSid': f.ACCOUNT, 'CallSid': f.CALL, 'From': '+12025550101', 'To': '+12025550100', 'Direction': 'inbound', **fields}
        signed = 'https://api.example' + path + ''.join(k + data[k] for k in sorted(data))
        signature = base64.b64encode(hmac.new(b'test-secret', signed.encode(), hashlib.sha1).digest()).decode()
        return r.handler({'rawPath': path, 'body': urllib.parse.urlencode(data), 'headers': {'x-twilio-signature': signature},
            'requestContext': {'http': {'method': 'POST'}}}, None)

    def control(self, action='redeem'):
        raw = json.dumps({'capability': self.item()['callbackId'], 'callSid': f.CALL}).encode()
        return i.redeem(raw, {'x-ln-signature': r.digest(('inbound-owner/' + action + '\n').encode() + raw)}, action)

    def test_missing_configuration_stop_unknown_caller_and_budget_fail_closed(self):
        self.assertIn('<Hangup', self.callback()['body'])
        self.configure()
        self.assertIn('<Hangup', self.callback(From='+12025550999')['body'])
        self.provider.assert_not_called()
        r._table.put_item(Item={**m.VOICE_KEY, 'activeRequest': 'owner-stop-2026-10-16'})
        self.assertIn('<Hangup', self.callback()['body'])
        self.provider.assert_not_called()
        r._table.items.pop((m.VOICE_KEY['PK'], m.VOICE_KEY['SK']))
        cfg = r.get(m.CONFIG_KEY); cfg['budgetFloorUSD'] = 20
        cfg['budgetFloorMonth'] = m.budget_clock(cfg)[0]; r._table.put_item(Item=cfg)
        self.assertIn('<Hangup', self.callback()['body'])
        self.assertIsNone(self.item())
        self.assertEqual(self.provider.call_count, 1)
        self.broker.assert_not_called()

    def test_pin_consent_and_single_use_bootstrap_preserve_identity_and_recording(self):
        self.configure()
        self.assertIn('six digit', self.callback()['body'])
        cap = self.provider.call_args.args[1]
        self.assertEqual(cap['TimeLimit'], 180)
        self.assertIn('Press 1', self.callback('pin/1', Digits='123456')['body'])
        self.broker.assert_not_called()
        reply = self.callback('consent', Digits='1')
        self.assertIn('wss://bridge.example/owner-media', reply['body'])
        self.assertNotIn('test-capability', reply['body'])
        self.broker.assert_called_once()
        call = r.get(r.call_key(f.CALL, m.namespace(owners.OWNER)))
        self.assertEqual(call['recordingStatus'], 'recording')
        self.assertNotIn('123456', json.dumps(list(r._table.items.values()), default=str))
        bootstrap = json.loads(self.control()['body'])
        self.assertEqual(bootstrap['sessionId'], 'fresh-session')
        self.assertEqual(self.control()['statusCode'], 403)
        self.assertEqual(self.control('check')['statusCode'], 200)
        r._table.put_item(Item={**m.VOICE_KEY, 'activeRequest': 'owner-stop-2026-10-16'})
        self.assertEqual(self.control('check')['statusCode'], 403)
        self.assertEqual(self.control('finish')['statusCode'], 200)
        self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'], 'owner-stop-2026-10-16')
        self.final.assert_called_once()

    def test_pin_attempt_replay_and_three_rejections_never_mint_or_record(self):
        self.configure(); self.callback()
        self.assertIn('six digit', self.callback('pin/1', Digits='000000')['body'])
        self.assertIn('<Hangup', self.callback('pin/1', Digits='123456')['body'])
        self.assertEqual(self.item()['pinAttempts'], 1)
        self.callback('pin/2', Digits='000000')
        self.assertIn('<Hangup', self.callback('pin/3', Digits='000000')['body'])
        self.assertEqual(self.item()['endReason'], 'pin_rejected')
        self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))
        self.broker.assert_not_called()
        self.assertFalse(any('/Recordings' in call.args[0] for call in self.provider.call_args_list))

    def test_ambiguous_mint_never_retries(self):
        self.configure(); self.callback(); self.callback('pin/1', Digits='123456')
        self.broker.side_effect = TimeoutError()
        self.assertIn('<Hangup', self.callback('consent', Digits='1')['body'])
        self.callback('consent', Digits='1')
        self.broker.assert_called_once()
        self.assertEqual(self.item()['inboundState'], 'ended')

    def test_untrusted_forms_and_iam_enrollment_do_not_enable_calls(self):
        self.configure()
        self.assertEqual(i.callback('/inbound-owner/voice', b'Digits=123456', {})['statusCode'], 403)
        before = r.get(m.CONFIG_KEY)
        event = {'operation': 'owner_inbound_enroll', 'ownerUserId': owners.OWNER, 'pinSalt': '2'*32, 'pinHash': '3'*64,
                 'expectedVersion': 1, 'replaceExisting': True}
        self.assertEqual(r.handler({**event, 'requestContext': {}}, None)['statusCode'], 404)
        self.assertEqual(r.handler(event, None)['statusCode'], 200)
        self.assertFalse(r.get(i.CONFIG)['enabled'])
        self.assertEqual(r.get(m.CONFIG_KEY), before)
        self.assertEqual(r.handler(event, None)['statusCode'], 409)

    def test_consent_refusal_and_duplicate_callbacks_do_not_record(self):
        self.configure(); self.callback()
        reservations = r.get(m.budget_keys(*m.budget_clock(r.get(m.CONFIG_KEY))[:2])[0])
        self.callback()
        self.assertEqual(r.get(m.budget_keys(*m.budget_clock(r.get(m.CONFIG_KEY))[:2])[0]), reservations)
        self.callback('pin/1', Digits='123456')
        self.assertIn('<Hangup', self.callback('consent', Digits='2')['body'])
        self.callback('consent', Digits='1')
        self.assertEqual(self.item()['endReason'], 'consent_declined')
        self.broker.assert_not_called()
        self.assertFalse(any('/Recordings' in call.args[0] for call in self.provider.call_args_list))

    def test_expired_auth_and_session_deadline_never_mint(self):
        self.configure(); self.callback()
        self.now.return_value = 1030
        self.assertIn('<Hangup', self.callback('pin/1', Digits='123456')['body'])
        self.assertEqual(self.item()['pinAttempts'], 0)
        self.now.return_value = 1180
        self.assertIn('<Hangup', self.callback('pin/1', Digits='123456')['body'])
        self.broker.assert_not_called()

    def test_invalid_account_destination_and_caller_do_not_advance_pin(self):
        self.configure(); self.callback()
        for fields in ({'AccountSid': 'AC'+'9'*32}, {'From': '+12025550999'}, {'To': '+12025550999'}):
            self.assertEqual(self.callback('pin/1', Digits='123456', **fields)['statusCode'], 403)
        self.assertEqual(self.item()['pinAttempts'], 0)

    def test_terminal_failure_is_not_reported_as_completed_and_unbound_stop_keeps_lock(self):
        self.configure(); self.callback()
        self.provider.side_effect = lambda *args: {'sid': 'CA'+'9'*32, 'account_sid': f.ACCOUNT, 'status': 'completed'}
        self.assertFalse(i.end(self.item(), 'test_failure', hangup=True))
        self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'], self.item()['lockId'])
        self.callback('status', CallStatus='failed')
        self.assertEqual(self.item()['providerStatus'], 'failed')
        self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))

    def test_enrollment_requires_explicit_replacement_and_current_version(self):
        self.configure()
        event = {'ownerUserId': owners.OWNER, 'pinSalt': '2'*32, 'pinHash': '3'*64, 'expectedVersion': 1}
        original = r.get(i.CONFIG)
        self.assertEqual(i.enroll(event)['statusCode'], 409)
        self.assertEqual(i.enroll({**event, 'expectedVersion': 0, 'replaceExisting': True})['statusCode'], 409)
        self.assertEqual(r.get(i.CONFIG), original)

    def test_finished_bridge_does_not_repeat_transcript_final_on_terminal_callback(self):
        self.configure(); self.callback(); self.callback('pin/1', Digits='123456'); self.callback('consent', Digits='1')
        self.control()
        raw = json.dumps({'capability': self.item()['callbackId'], 'callSid': f.CALL,
                          'transcriptFinalized': True, 'reason': 'remote_hangup', 'inputTokens': 123, 'outputTokens': 45}).encode()
        self.assertEqual(i.redeem(raw, {'x-ln-signature': r.digest(b'inbound-owner/finish\n' + raw)}, 'finish')['statusCode'], 200)
        self.callback('status', CallStatus='completed')
        self.final.assert_not_called()
        self.assertTrue(self.item()['transcriptFinalized'])
        self.assertEqual(self.item()['endReason'], 'remote_hangup')
        self.assertEqual(self.item()['inputTokens'], 123)

    def test_fresh_account_revocation_blocks_media_heartbeat_and_enrollment(self):
        self.configure(); self.callback(); self.callback('pin/1', Digits='123456'); self.callback('consent', Digits='1')
        self.control()
        self.owner_active.return_value = False
        self.assertEqual(self.control('check')['statusCode'], 403)
        self.owner_active.assert_called_with(owners.OWNER, since=1000)
        self.assertEqual(i.enroll({'ownerUserId': owners.OWNER})['statusCode'], 403)

    def test_control_signature_cannot_be_replayed_across_actions(self):
        self.configure(); self.callback(); self.callback('pin/1', Digits='123456'); self.callback('consent', Digits='1')
        raw = json.dumps({'capability': self.item()['callbackId'], 'callSid': f.CALL}).encode()
        before = self.item()
        for signed_action in ('redeem', 'check', 'finish'):
            signature = r.digest(('inbound-owner/' + signed_action + '\n').encode() + raw)
            for replay_action in ('redeem', 'check', 'finish'):
                if replay_action != signed_action:
                    self.assertEqual(i.redeem(raw, {'x-ln-signature': signature}, replay_action)['statusCode'], 403)
        self.assertEqual(self.item(), before)
        self.assertEqual(i.redeem(raw, {'x-ln-signature': r.digest(raw)}, 'redeem')['statusCode'], 403)

    def test_dynamodb_decimal_deadline_keeps_integer_bootstrap_wire_type(self):
        self.configure(); self.callback(); self.callback('pin/1', Digits='123456'); self.callback('consent', Digits='1')
        stored = self.item()
        stored['deadline'] = Decimal('1180')
        r._table.put_item(Item=stored)
        reply = self.control()
        self.assertEqual(reply['statusCode'], 200)
        deadline = json.loads(reply['body'])['deadline']
        self.assertIs(type(deadline), int)
        self.assertEqual(deadline, 1180)
        numeric = json.loads(r.response(200, {'integer': Decimal('42.000'), 'fraction': Decimal('1.40')})['body'])
        self.assertIs(type(numeric['integer']), int)
        self.assertEqual(numeric['fraction'], 1.4)

    def test_broker_invocation_uses_exact_trusted_identity_and_wire_contract(self):
        self.configure(); self.callback()
        provider = Mock()
        bootstrap = {'mode': 'phone', 'sessionId': 'fresh-session', 'bridgeToken': 'private-test-token',
                     'sessionConfig': {'type': 'realtime', 'instructions': 'Owner context', 'tools': []}}
        provider.invoke.return_value = {'Payload': io.BytesIO(json.dumps(bootstrap).encode())}
        fake_boto = types.SimpleNamespace(client=Mock(return_value=provider))
        with patch.dict(sys.modules, {'boto3': fake_boto}):
            self.assertEqual(REAL_MINT(self.item()), bootstrap)
        args = provider.invoke.call_args.kwargs
        self.assertEqual(args['FunctionName'], 'live-ninja-realtime-broker')
        self.assertEqual(json.loads(args['Payload']), {'mode': 'phone-session', 'userId': owners.OWNER,
            'surface': 'phone', 'phoneCallSid': f.CALL, 'phoneDeadline': 1180})


class EnrollmentHelperTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        path = Path(__file__).resolve().parents[2] / 'scripts' / 'enroll-phone-pin.py'
        spec = importlib.util.spec_from_file_location('phone_pin_helper', path)
        cls.helper = importlib.util.module_from_spec(spec); spec.loader.exec_module(cls.helper)

    def invoke(self, *, existing=False, args=(), pin='123456', interactive=True, repository='JeremyProffittOrg/live-ninja', account='759775734231'):
        session, table, main, sts, function = Mock(), Mock(), Mock(), Mock(), Mock()
        owner = owners.OWNER
        table.get_item.side_effect = [{'Item': {'ownerUserId': owner}}, {'Item': {'pinHash': 'x', 'version': 4}} if existing else {}]
        main.get_item.return_value = {'Item': {'userId': owner}}
        session.resource.return_value.Table.side_effect = lambda name: main if name == 'live-ninja' else table
        sts.get_caller_identity.return_value = {'Account': account}
        session.client.side_effect = lambda name: sts if name == 'sts' else function
        function.invoke.return_value = {'Payload': io.BytesIO(json.dumps({'statusCode': 200, 'body': json.dumps({'enrolled': True, 'enabled': False})}).encode())}
        fake_boto = types.SimpleNamespace(Session=Mock(return_value=session))
        output = io.StringIO()
        with patch.dict(sys.modules, {'boto3': fake_boto}), patch.object(sys, 'argv', ['helper', *args]), \
             patch.object(sys.stdin, 'isatty', return_value=interactive), patch.object(sys, 'stdout', output), \
             patch.object(self.helper.subprocess, 'run', return_value=types.SimpleNamespace(returncode=0, stdout=repository)) as process, \
             patch.object(self.helper.getpass, 'getpass', return_value=pin) as prompt:
            result = self.helper.main()
        self.assertNotIn(pin, output.getvalue()) if pin else None
        return result, function, prompt, process

    def test_existing_empty_and_noninteractive_inputs_never_write(self):
        for values in ({'existing': True}, {'pin': ''}, {'interactive': False}, {'repository': 'other/repo'}, {'account': '000000000000'}):
            with self.subTest(values=values):
                _, function, prompt, _ = self.invoke(**values)
                function.invoke.assert_not_called()
                if values.get('pin') != '':
                    prompt.assert_not_called()

    def test_explicit_replacement_sends_only_salted_verifier_with_expected_version(self):
        result, function, prompt, process = self.invoke(existing=True, args=['--replace-existing'])
        self.assertEqual(result, 0); prompt.assert_called_once()
        payload = json.loads(function.invoke.call_args.kwargs['Payload'])
        self.assertEqual(payload['expectedVersion'], 4)
        self.assertTrue(payload['replaceExisting'])
        self.assertNotIn('123456', json.dumps(payload))
        self.assertEqual(payload['pinHash'], hashlib.pbkdf2_hmac('sha256', b'123456', bytes.fromhex(payload['pinSalt']), 600000).hex())
        self.assertNotIn('123456', str(process.call_args))

    def test_invalid_pin_and_arguments_do_not_write(self):
        for values in ({'pin': '123'}, {'args': ['--pin', '123456']}):
            result, function, _, _ = self.invoke(**values)
            self.assertEqual(result, 1); function.invoke.assert_not_called()


if __name__ == '__main__':
    unittest.main()
