import base64
import hashlib
import hmac
import json
import io
import os
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
        r._table.put_item(Item={**i.CONFIG, 'version': 1, 'enabled': True, 'ownerUserId': owners.OWNER})
        self.addCleanup(patch.stopall)
        patch.dict(os.environ, {'INBOUND_BRIDGE_URL': 'https://bridge.example'}).start()
        self.owner_active = patch('account_lifecycle.owner_active', return_value=True).start()
        patch.object(m, 'transact', self.transact).start()
        self.provider = patch.object(m, 'provider', side_effect=self.provider_result).start()
        self.broker = patch.object(i, 'mint', return_value={'mode': 'phone', 'bridgeToken': 'test-capability',
            'sessionId': 'fresh-session', 'sessionConfig': {'instructions': 'owner context', 'tools': []}}).start()
        self.final = patch.object(i, 'final_transcript').start()
        self.canonical = Mock()
        self.canonical.get_item.return_value = {'Item': {'userId': owners.OWNER}}
        resource = Mock(); resource.Table.return_value = self.canonical
        patch.dict(sys.modules, {'boto3': types.SimpleNamespace(resource=Mock(return_value=resource))}).start()

    def transact(self, operations):
        regular = []
        for operation in operations:
            check = operation.get('ConditionCheck', {})
            if check.get('Key') == i.CONFIG:
                cfg = r.get(i.CONFIG) or {}
                values = check['ExpressionAttributeValues']
                if cfg.get('enabled') is not True or any(cfg.get(field) != values[name] for field, name in
                    (('ownerUserId', ':owner'), ('version', ':version'))):
                    raise f.Conflict()
                if (':daily' in values and cfg.get('dailyBudget') != values[':daily']) or (':daily' not in values and 'dailyBudget' in cfg):
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

    def configure_daily(self):
        self.configure()
        cfg = r.get(m.CONFIG_KEY)
        cfg.update(timezone='America/New_York', budgetFloorUSD=Decimal('20'), budgetFloorMonth='2026-10')
        r._table.put_item(Item=cfg)
        inbound = r.get(i.CONFIG)
        inbound['dailyBudget'] = {'enabled': True, 'callerNumber': cfg['destinations']['primary'],
                                  'limitUSD': Decimal('10'), 'version': Decimal('1')}
        r._table.put_item(Item=inbound)
        self.budget_clock = patch.object(m, 'budget_clock', return_value=('2026-10', '2026-10-18', 'America/New_York', 2000)).start()
        r._table.put_item(Item={**m.budget_keys('2026-10', '2026-10-18')[0], 'version': 1, 'allocatedUSD': Decimal('20')})

    def incoming_day(self, day='2026-10-18'):
        return r.get(m.inbound_budget_key(owners.OWNER, day))

    def callback(self, kind='voice', **fields):
        path = '/inbound-owner/' + kind
        if kind not in {'voice', 'status'}:
            path += '/' + self.item()['callbackId']
        data = {'AccountSid': f.ACCOUNT, 'CallSid': f.CALL, 'From': '+12025550101', 'To': '+12025550100', 'Direction': 'inbound', **fields}
        data = {key: value for key, value in data.items() if value is not None}
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
        # The $100 monthly cap covers incoming calls too: $0.40 left cannot fund the $0.459 telephone reserve.
        cfg = r.get(m.CONFIG_KEY); cfg['budgetFloorUSD'] = Decimal('99.60')
        cfg['budgetFloorMonth'] = m.budget_clock(cfg)[0]; r._table.put_item(Item=cfg)
        self.assertIn('<Hangup', self.callback()['body'])
        self.assertIsNone(self.item())
        self.assertEqual(self.provider.call_count, 1)
        self.broker.assert_not_called()

    def test_outbound_live_delivery_reuses_bound_owner_session_and_cleanup(self):
        self.configure()
        event = {'operation':'owner_message_send', 'ownerUserId':owners.OWNER, 'requestId':'live-delivery',
                 'destination':'primary', 'message':'Sales update', 'live':True, 'payload':'{"total":12}'}
        reply = r.handler(event, None)
        self.assertEqual(reply['statusCode'], 200)
        self.item = lambda: r.get(m.request_key(owners.OWNER, 'live-delivery'))
        self.assertEqual(self.item()['reservedUSD'], Decimal('10'))
        self.assertEqual(self.item()['costCapUSD'], Decimal('9.541'))
        self.assertEqual(self.item()['telephoneReserveUSD'], m.TELEPHONE_RESERVE)
        create = next(call.args[1] for call in self.provider.call_args_list if call.args[0] == '/Calls.json')
        self.assertNotIn('Record',create)
        self.assertIn('/inbound-owner/connect/',create['Twiml'])
        self.assertIn('being recorded',create['Twiml'])
        self.assertNotIn('Sales update',create['Twiml'])
        self.assertEqual(r.handler(event,None)['statusCode'],200)
        self.assertEqual(sum(call.args[0]=='/Calls.json' for call in self.provider.call_args_list),1)
        self.assertEqual(r.handler({**event,'payload':'{"total":13}'},None)['statusCode'],409)
        self.assertEqual(self.callback('connect')['statusCode'],403)
        connected = self.callback('connect',From='+12025550100',To='+12025550101',Direction='outbound-api')
        self.assertIn('<Connect',connected['body'])
        self.assertEqual(self.item()['recordingAuthorization'],'owner-message')
        self.assertNotIn('<Connect',self.callback('connect',From='+12025550100',To='+12025550101',Direction='outbound-api')['body'])
        self.assertEqual(self.broker.call_count,1)
        self.assertEqual(self.broker.call_args.args[0]['payload'],'{"total":12}')
        self.assertEqual(self.control()['statusCode'],200)
        self.assertEqual(self.control('check')['statusCode'],200)
        self.assertEqual(self.control('finish')['statusCode'],200)
        self.assertEqual(self.item()['inboundState'],'ended')
        self.assertNotIn('activeRequest',r.get(m.VOICE_KEY))

    def test_outbound_payload_rejected_before_provider_or_reservation(self):
        self.configure()
        for fields in ({'payload':'{}'}, {'live':True,'payload':'[]'}, {'live':True,'payload':'{"x":NaN}'}, {'live':True,'payload':'x'*12001}):
            result=r.handler({'operation':'owner_message_send','ownerUserId':owners.OWNER,'requestId':'invalid-live',
                              'destination':'primary','message':'Summary',**fields},None)
            self.assertEqual(result['statusCode'],400)
        self.provider.assert_not_called()

    def test_outbound_live_call_keeps_its_cap_under_the_standing_allowance(self):
        self.configure()
        cfg=r.get(m.CONFIG_KEY)
        cfg.update(budgetFloorUSD=Decimal('20'),budgetFloorMonth='2026-10')
        r._table.put_item(Item=cfg)
        patch.object(m,'budget_clock',return_value=('2026-10','2026-10-18','America/New_York',1100)).start()
        self.item=lambda:r.get(m.request_key(owners.OWNER,'bounded-live'))
        result=r.handler({'operation':'owner_message_send','ownerUserId':owners.OWNER,'requestId':'bounded-live',
                          'destination':'primary','message':'Summary','live':True},None)
        self.assertEqual(result['statusCode'],200)
        # The reservation is charged to the day it was admitted; it does not shorten the call.
        self.assertEqual(r.get(m.budget_keys('2026-10','2026-10-18')[1])['reservedUSD'],Decimal('10'))
        self.assertNotIn('budgetExpiresAt',self.item())
        self.assertEqual(self.item()['deadline'],1600)
        create=next(call.args[1] for call in self.provider.call_args_list if call.args[0]=='/Calls.json')
        self.assertEqual(create['TimeLimit'],600,'owner decision 2026-10-19: a 10 minute limit per call')
        self.assertIn('<Connect',self.callback('connect',From='+12025550100',To='+12025550101',Direction='outbound-api')['body'])
        caps=[call.args[1]['TimeLimit'] for call in self.provider.call_args_list if call.args[0].endswith('/'+f.CALL+'.json') and 'TimeLimit' in call.args[1]]
        self.assertEqual(caps,[600], 'the per-call cap is 600 seconds (owner decision 2026-10-19), set after answer')

    def test_unknown_caller_only_hangs_up_without_reservation_even_while_stopped(self):
        self.assertEqual(self.callback(From='+12025550999')['body'], '<Response><Hangup/></Response>')
        self.configure()
        for enabled in (True, False):
            cfg=r.get(m.CONFIG_KEY);cfg['enabled']=enabled;r._table.put_item(Item=cfg)
            r._table.put_item(Item={**m.VOICE_KEY,'activeRequest':'owner-stop'})
            before=dict(r._table.items)
            reply=self.callback(From='+12025550999')
            self.assertEqual(reply['body'],'<Response><Hangup/></Response>')
            self.assertEqual(r._table.items,before)
        self.provider.assert_not_called();self.broker.assert_not_called()

    def test_both_configured_owner_aliases_automatically_connect_without_keypad(self):
        for caller in ('+12025550101','+12025550102'):
            with self.subTest(caller=caller):
                r._table=f.Table();self.configure()
                self.assertNotIn('pinHash',r.get(i.CONFIG));self.assertNotIn('pinSalt',r.get(i.CONFIG))
                first=self.callback(From=caller)
                self.assertIn('<Redirect method="POST">',first['body']);self.assertNotIn('<Gather',first['body']);self.assertIn('Connecting to Live Ninja.',first['body']);self.assertNotIn('PIN',first['body'])
                self.assertEqual(self.item()['inboundState'],'connecting')
                self.assertEqual(self.item()['connectDeadline'],1030)
                self.assertNotIn('pinAttempts',self.item())
                self.broker.assert_not_called()
                self.assertIn('<Connect',self.callback('connect',From=caller)['body'])
                self.broker.assert_called_once()

    def test_old_pin_routes_are_removed_and_unsigned_callbacks_rejected(self):
        self.configure();self.callback()
        before=self.item()
        self.assertEqual(self.callback('pin/1',Digits='123456')['statusCode'],404)
        self.assertEqual(self.callback('consent')['statusCode'],404)
        self.assertEqual(i.callback('/inbound-owner/voice',b'',{})['statusCode'],403)
        self.assertEqual(self.item(),before)
        self.broker.assert_not_called()

    def test_invalid_inbound_config_version_fails_closed_without_budget(self):
        self.configure();original=r.get(i.CONFIG)
        for version in (None,True,0,Decimal('1.5'),Decimal('NaN'),Decimal('Infinity')):
            value={**original,'version':version}
            if version is None:value.pop('version')
            r._table.put_item(Item=value)
            self.assertIn('<Hangup',self.callback()['body']);self.assertIsNone(self.item())
        self.provider.assert_not_called()
        r._table.put_item(Item={**original,'version':Decimal('1')})
        self.assertIn('Connecting to Live Ninja.',self.callback()['body'])

    def test_caller_revoked_after_gather_cannot_mint_owner_session(self):
        self.configure();self.callback()
        cfg=r.get(m.CONFIG_KEY);cfg['destinations']['primary']='+12025550999';r._table.put_item(Item=cfg)
        self.assertIn('<Hangup',self.callback('connect')['body'])
        self.assertEqual(self.item()['endReason'],'admission_revoked')
        self.broker.assert_not_called()
        self.assertFalse(any('/Recordings' in call.args[0] for call in self.provider.call_args_list))

    def test_caller_revocation_blocks_active_media_heartbeat(self):
        self.configure();self.callback();self.callback('connect');self.control()
        cfg=r.get(m.CONFIG_KEY);cfg['destinations']['primary']='+12025550999';r._table.put_item(Item=cfg)
        self.assertEqual(self.control('check')['statusCode'],403)

    def test_caller_connection_and_single_use_bootstrap_preserve_identity_and_recording(self):
        self.configure()
        self.assertIn('Connecting to Live Ninja.', self.callback()['body'])
        self.assertFalse(any(call.args[1] for call in self.provider.call_args_list if len(call.args)>1))
        self.broker.assert_not_called()
        reply = self.callback('connect')
        self.assertIn('wss://bridge.example/owner-media', reply['body'])
        self.assertNotIn('test-capability', reply['body'])
        self.broker.assert_called_once()
        call = r.get(r.call_key(f.CALL, m.namespace(owners.OWNER)))
        self.assertEqual(call['recordingStatus'], 'recording')
        self.assertNotIn('pinAttempts', self.item())
        bootstrap = json.loads(self.control()['body'])
        self.assertEqual(bootstrap['sessionId'], 'fresh-session')
        self.assertEqual(self.control()['statusCode'], 403)
        self.assertEqual(self.control('check')['statusCode'], 200)
        r._table.put_item(Item={**m.VOICE_KEY, 'activeRequest': 'owner-stop-2026-10-16'})
        self.assertEqual(self.control('check')['statusCode'], 403)
        self.assertEqual(self.control('finish')['statusCode'], 200)
        self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'], 'owner-stop-2026-10-16')
        self.final.assert_called_once()

    def test_inbound_cap_is_armed_only_after_initial_gather_answers(self):
        self.configure()
        answered = False
        caps = []
        def provider_state(path, form=None):
            if form and 'TimeLimit' in form:
                if not answered:
                    raise m.ProviderError(21220, rejected=True)
                caps.append(form['TimeLimit'])
            return self.provider_result(path, form)
        self.provider.side_effect = provider_state
        initial = self.callback()
        self.assertIn('Connecting to Live Ninja.', initial['body'])
        self.assertIn('<Redirect method="POST">', initial['body'])
        self.assertNotIn('<Gather', initial['body'])
        self.assertLess(initial['body'].index('<Say'), initial['body'].index('<Redirect'))
        self.assertIn('<Hangup/>', initial['body'])
        self.assertNotIn('PIN', initial['body'])
        self.assertEqual(caps, [])
        answered = True
        self.now.return_value = 1005
        self.assertIn('<Connect', self.callback('connect')['body'])
        self.assertEqual(caps, [595])
        self.now.return_value = 1010
        self.assertIn('<Hangup', self.callback('connect')['body'])
        self.assertEqual(caps, [595])
        self.assertEqual(self.item()['deadline'], 1600)
        self.broker.assert_called_once()

    def test_connect_cap_rejection_stops_before_mint_and_records_only_code(self):
        self.configure(); self.callback()
        def reject_cap(path, form=None):
            if form and 'TimeLimit' in form:
                error = m.ProviderError(21220, rejected=True)
                error.args = ('RAW_PROVIDER_RESPONSE_MUST_NOT_BE_STORED',)
                raise error
            return self.provider_result(path, form)
        self.provider.side_effect = reject_cap
        self.assertIn('<Hangup', self.callback('connect')['body'])
        self.assertEqual(self.item()['inboundState'], 'ended')
        self.assertEqual(self.item()['endReason'], 'provider_cap_failed_21220')
        self.assertNotIn('providerCapSet', self.item())
        self.assertTrue(self.item()['consented'])
        self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))
        self.broker.assert_not_called()
        self.assertFalse(any('/Recordings' in call.args[0] for call in self.provider.call_args_list))
        self.assertNotIn('RAW_PROVIDER_RESPONSE', json.dumps(list(r._table.items.values()), default=str))

    def test_connect_cap_response_identity_and_state_fail_closed(self):
        for bad in ({'sid':'CA'+'9'*32}, {'account_sid':'AC'+'9'*32}, {'status':'ringing'}):
            with self.subTest(response=bad):
                r._table = f.Table()
                self.configure(); self.callback()
                def bad_cap(path, form=None):
                    result = self.provider_result(path, form)
                    return {**result, **bad} if form and 'TimeLimit' in form else result
                self.provider.side_effect = bad_cap
                self.assertIn('<Hangup', self.callback('connect')['body'])
                self.assertEqual(self.item()['endReason'], 'provider_cap_failed')
                self.assertNotIn('providerCapSet', self.item())
                self.assertTrue(self.item()['consented'])
                self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))
                self.broker.assert_not_called()

    def test_recurring_incoming_allowance_survives_monthly_limit_without_generic_grant(self):
        self.configure_daily()
        self.assertIn('<Redirect', self.callback()['body'])
        self.assertEqual(self.incoming_day()['reservedUSD'], Decimal('10'))
        self.assertEqual(self.item()['costCapUSD'], Decimal('9.541'))
        self.assertEqual(self.item()['budgetExpiresAt'], 2000)
        self.assertEqual(self.item()['budgetSource'], 'incoming-daily-policy')
        self.assertIsNone(r.get(m.budget_keys('2026-10', '2026-10-18')[1]))
        self.assertEqual(r.get(m.budget_keys('2026-10', '2026-10-18')[0])['allocatedUSD'], Decimal('30'))
        self.callback('status', CallStatus='completed')
        self.callback()  # Same CallSid cannot reserve again even after releasing its voice lock.
        self.assertEqual(self.incoming_day()['reservedUSD'], Decimal('10'))
        status = json.loads(r.handler({'operation':'owner_budget_status','ownerUserId':owners.OWNER}, None)['body'])
        self.assertEqual((status['dailyReservedUSD'], status['dailyRemainingUSD']), ('0', '10'), 'incoming calls do not use the outgoing allowance')
        self.assertEqual(status['pausedReason'], '')
        self.assertEqual(status['incomingDailyBudget'], {'enabled': True, 'limitUSD':'10', 'reservedUSD':'10',
            'remainingUSD':'0', 'day':'2026-10-18', 'timezone':'America/New_York', 'expiresAt':2000})
        self.assertEqual((status['perCallCapUSD'], status['perCallMaxSeconds'], status['telephoneReserveUSD'], status['requiredReservationUSD']),
                         ('10', 600, '0.4590', '10'))
        self.assertNotIn('callerNumber', json.dumps(status)); self.assertNotIn('+12025550101', json.dumps(status))
        self.assertNotIn('pinHash', json.dumps(status)); self.assertNotIn('pinSalt', json.dumps(status))

    def test_incoming_calls_count_toward_the_monthly_cap(self):
        self.configure_daily()
        r._table.put_item(Item={**m.budget_keys('2026-10', '2026-10-18')[0], 'version': 2, 'allocatedUSD': Decimal('99.60'),
                                'ledgerVersion': m.LEDGER_VERSION, 'unsettled': {}})
        body = self.callback()['body']
        self.assertIn("monthly call allowance is used. Calls resume next month", body)
        self.assertIn('<Hangup', body)
        self.assertIsNone(self.item())
        self.assertIsNone(self.incoming_day())
        self.assertIsNone(r.get(m.VOICE_KEY))
        self.broker.assert_not_called()

    def test_recurring_allowance_does_not_fund_other_caller_outbound_or_hotel(self):
        self.configure_daily()
        # Calls outside the incoming policy draw only on the standing allowance, here used up.
        r._table.put_item(Item={**m.budget_keys('2026-10', '2026-10-18')[1], 'version': 1, 'reservedUSD': Decimal('9.90'), 'expiresAt': 2000})
        self.assertIn('<Hangup', self.callback(From='+12025550102')['body'])
        self.assertIsNone(self.incoming_day())
        outgoing = r.handler({'operation':'owner_message_send','ownerUserId':owners.OWNER,'requestId':'outbound-policy-test',
                              'destination':'primary','message':'Test'}, None)
        self.assertEqual(outgoing['statusCode'], 409)
        cfg = r.get(m.CONFIG_KEY)
        with self.assertRaisesRegex(ValueError, 'daily_allowance_used'):
            m.reserve(owners.OWNER, 'hotel-policy-test', 'hotel-01', 'Research', cfg,
                      reserve_amount=Decimal('1.40'), target_number='+12025550109')
        self.assertIsNone(self.incoming_day())
        self.assertIsNone(r.get(m.VOICE_KEY))
        self.assertFalse(any(args.args[0] == '/Calls.json' for args in self.provider.call_args_list))

    def test_recurring_daily_cap_counts_full_reserve_and_does_not_fall_back(self):
        self.configure_daily()
        manual_key = m.budget_keys('2026-10', '2026-10-18')[1]
        manual = {**manual_key,'version':1,'grantedUSD':Decimal('100'),'spentUSD':Decimal('0'),'expiresAt':2000}
        r._table.put_item(Item=manual)
        original = self.provider_result
        prices = {}
        def dynamic_call(path, form=None):
            if path.startswith('/Calls/') and path.endswith('.json') and '/Recordings' not in path:
                sid = path.split('/')[2].split('.')[0]
                result = {'sid':sid, 'account_sid':f.ACCOUNT,
                          'status':'completed' if form is None or form.get('Status') == 'completed' else 'in-progress'}
                if form is None and sid in prices:
                    result.update(price=prices[sid], price_unit='USD', duration='65')
                return result
            return original(path, form)
        self.provider.side_effect = dynamic_call
        first = 'CA' + format(1, '032x')
        self.assertIn('<Redirect', self.callback(CallSid=first)['body'])
        # A full-length conversation reserves the whole remaining incoming allowance.
        self.assertEqual(self.incoming_day()['reservedUSD'], Decimal('10'))
        self.callback('status', CallSid=first, CallStatus='completed')
        exhausted = self.callback(CallSid='CA'+format(2,'032x'))
        self.assertIn('Please call again tomorrow', exhausted['body'])
        self.assertEqual(self.incoming_day()['reservedUSD'], Decimal('10'))
        self.assertEqual(r.get(manual_key), manual)
        self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))
        # The bridge finish prices the model; settlement replaces the reservation with the measured cost.
        raw = json.dumps({'capability': r.get(m.request_key(owners.OWNER, 'inbound-' + first))['callbackId'], 'callSid': first,
                          'transcriptFinalized': True, 'reason': 'remote_hangup', 'inputTokens': 1200, 'outputTokens': 300,
                          'modelCostUSD': '0.250000'}).encode()
        self.assertEqual(i.redeem(raw, {'x-ln-signature': r.digest(b'inbound-owner/finish\n' + raw)}, 'finish')['statusCode'], 200)
        prices[first] = '-0.0170'
        m.budget_status(r.get(m.CONFIG_KEY))
        # 65 s -> 2 started minutes: 2 * 0.0209 + 0.25 = 0.2918 telephone, plus 0.25 model.
        self.assertEqual(self.incoming_day()['reservedUSD'], Decimal('0.5418'))
        self.assertEqual(r.get(m.request_key(owners.OWNER, 'inbound-' + first))['settledUSD'], Decimal('0.5418'))
        self.assertEqual(r.get(m.budget_keys('2026-10', '2026-10-18')[0])['allocatedUSD'], Decimal('20.5418'))
        # The freed allowance admits the next call with a smaller cap.
        third = 'CA'+format(3,'032x')
        self.assertIn('<Redirect', self.callback(CallSid=third)['body'])
        self.assertEqual(self.incoming_day()['reservedUSD'], Decimal('10'))
        self.assertEqual(r.get(m.request_key(owners.OWNER, 'inbound-' + third))['costCapUSD'], Decimal('10') - Decimal('0.5418') - m.TELEPHONE_RESERVE)
        self.callback('status', CallSid=third, CallStatus='completed')
        self.budget_clock.return_value = ('2026-10', '2026-10-19', 'America/New_York', 4000)
        self.now.return_value = 3000
        self.assertIn('<Redirect', self.callback(CallSid='CA'+format(9,'032x'))['body'])
        self.assertEqual(self.incoming_day('2026-10-19')['reservedUSD'], Decimal('10'))
        self.assertEqual(self.incoming_day()['reservedUSD'], Decimal('10'))

    def test_policy_revocation_and_absence_are_fenced_in_reservation(self):
        self.configure_daily()
        transact = self.transact
        def revoke(operations):
            policy = r.get(i.CONFIG); policy['dailyBudget']['enabled'] = False; r._table.put_item(Item=policy)
            return transact(operations)
        with patch.object(m, 'transact', side_effect=revoke):
            self.assertIn('<Hangup', self.callback()['body'])
        self.assertIsNone(self.incoming_day()); self.assertIsNone(self.item()); self.assertIsNone(r.get(m.VOICE_KEY))
        # A newly added policy must also prevent a stale manual-path reservation.
        inbound = r.get(i.CONFIG); inbound.pop('dailyBudget'); r._table.put_item(Item=inbound)
        cfg = r.get(m.CONFIG_KEY); cfg['budgetFloorUSD'] = Decimal('8'); r._table.put_item(Item=cfg)
        month = r.get(m.budget_keys('2026-10','2026-10-18')[0]); month['allocatedUSD'] = Decimal('8'); r._table.put_item(Item=month)
        def install(operations):
            policy = r.get(i.CONFIG); policy['dailyBudget'] = {'enabled':True,'callerNumber':'+12025550101','limitUSD':Decimal('10'),'version':1}
            r._table.put_item(Item=policy)
            return transact(operations)
        with patch.object(m,'transact',side_effect=install):
            self.assertIn('<Hangup',self.callback()['body'])
        self.assertIsNone(self.item()); self.assertIsNone(r.get(m.VOICE_KEY))
        self.assertEqual(r.get(m.budget_keys('2026-10','2026-10-18')[0])['allocatedUSD'], Decimal('8'))

    def test_malformed_matched_policy_never_uses_manual_allowance(self):
        self.configure_daily()
        original = r.get(i.CONFIG)
        r._table.put_item(Item={**m.budget_keys('2026-10','2026-10-18')[1], 'version':1,
            'grantedUSD':Decimal('100'),'spentUSD':Decimal('0'),'expiresAt':2000})
        for field,value in [('limitUSD','NaN'),('limitUSD','Infinity'),('limitUSD','11'),('version',True),('version',Decimal('1.5')),('version',0)]:
            item = {**original,'dailyBudget':{**original['dailyBudget'],field:value}}
            r._table.put_item(Item=item)
            with self.subTest(field=field,value=value):
                self.assertIn('<Hangup',self.callback()['body'])
                self.assertIsNone(self.incoming_day()); self.assertIsNone(self.item())

    def test_policy_reservation_still_obeys_stop_deletion_and_owner_binding(self):
        self.configure_daily()
        r._table.put_item(Item={**m.VOICE_KEY,'activeRequest':'owner-stop'})
        self.assertIn('<Hangup',self.callback()['body']); self.assertIsNone(self.incoming_day())
        r._table.items.pop((m.VOICE_KEY['PK'],m.VOICE_KEY['SK']))
        r._table.put_item(Item={'PK':'OWNER#'+owners.OWNER,'SK':'ACCOUNT_DELETION','ownerUserId':owners.OWNER})
        self.assertIn('<Hangup',self.callback()['body']); self.assertIsNone(self.incoming_day())

    def test_policy_counter_conflict_cannot_exceed_cap(self):
        self.configure_daily()
        key=m.inbound_budget_key(owners.OWNER,'2026-10-18')
        row={**key,'ownerUserId':owners.OWNER,'day':'2026-10-18','timezone':'America/New_York',
             'limitUSD':Decimal('10'),'reservedUSD':Decimal('8.40'),'expiresAt':2000,'version':1}
        r._table.put_item(Item=row)
        transact=self.transact
        def competing_reservation(operations):
            r._table.put_item(Item={**row,'reservedUSD':Decimal('9.80'),'version':2})
            return transact(operations)
        with patch.object(m,'transact',side_effect=competing_reservation):
            self.assertIn('Please call again tomorrow',self.callback()['body'])
        self.assertEqual(self.incoming_day()['reservedUSD'],Decimal('9.80'))
        self.assertIsNone(self.item()); self.assertIsNone(r.get(m.VOICE_KEY))

    def test_recurring_policy_version_change_does_not_reset_same_day_counter(self):
        self.configure_daily()
        key=m.inbound_budget_key(owners.OWNER,'2026-10-18')
        r._table.put_item(Item={**key,'ownerUserId':owners.OWNER,'day':'2026-10-18','timezone':'America/New_York',
            'limitUSD':Decimal('10'),'reservedUSD':Decimal('9.80'),'expiresAt':2000,'version':Decimal('7')})
        inbound=r.get(i.CONFIG); inbound['dailyBudget']['version']=Decimal('2'); r._table.put_item(Item=inbound)
        self.assertIn('Please call again tomorrow',self.callback()['body'])
        self.assertEqual(self.incoming_day()['reservedUSD'],Decimal('9.80'))
        self.assertIsNone(self.item())

    def test_recurring_allowance_reserves_full_amount_below_monthly_threshold(self):
        self.configure_daily()
        cfg=r.get(m.CONFIG_KEY); cfg.update(budgetFloorUSD=Decimal('8')); r._table.put_item(Item=cfg)
        monthly=r.get(m.budget_keys('2026-10','2026-10-18')[0]); monthly['allocatedUSD']=Decimal('8'); r._table.put_item(Item=monthly)
        self.assertIn('<Redirect',self.callback()['body'])
        self.assertEqual(self.incoming_day()['reservedUSD'],Decimal('10'))
        self.assertEqual(self.item()['budgetExpiresAt'],2000)
        self.assertEqual(r.get(m.budget_keys('2026-10','2026-10-18')[0])['allocatedUSD'],Decimal('18'))
        self.assertIsNone(r.get(m.budget_keys('2026-10','2026-10-18')[1]))

    def test_midnight_crossing_before_commit_makes_no_reservation(self):
        self.configure_daily()
        self.budget_clock.side_effect=[('2026-10','2026-10-18','America/New_York',2000),
            ('2026-10','2026-10-18','America/New_York',2000),('2026-10','2026-10-19','America/New_York',4000)]
        self.assertIn('<Hangup',self.callback()['body'])
        self.assertIsNone(self.incoming_day()); self.assertIsNone(self.item()); self.assertIsNone(r.get(m.VOICE_KEY))
        self.assertEqual(r.get(m.budget_keys('2026-10','2026-10-18')[0])['allocatedUSD'],Decimal('20'))

    def test_recurring_quota_renews_at_month_boundary_without_manual_grant(self):
        self.configure_daily()
        self.budget_clock.return_value=('2026-11','2026-11-01','America/New_York',4000)
        self.now.return_value=3000
        self.assertIn('<Redirect',self.callback()['body'])
        self.assertEqual(self.incoming_day('2026-11-01')['reservedUSD'],Decimal('10'))
        self.assertEqual(self.item()['inboundBudgetDay'],'2026-11-01')
        self.assertIsNone(r.get(m.budget_keys('2026-11','2026-11-01')[1]))

    def test_number_status_early_hangup_before_connect_releases_only_own_lock(self):
        self.configure(); self.callback()
        self.provider.reset_mock()
        self.assertEqual(self.callback('status', CallStatus='completed')['statusCode'], 200)
        self.assertEqual(self.item()['inboundState'], 'ended')
        self.assertEqual(self.item()['providerStatus'], 'completed')
        self.assertNotIn('pinAttempts', self.item())
        self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))
        self.assertEqual(r.get(r.call_key(f.CALL, m.namespace(owners.OWNER)))['recordingStatus'], 'absent')
        self.canonical.get_item.assert_called_with(Key={'pk':'CONFIG','sk':'OWNER'}, ProjectionExpression='userId', ConsistentRead=True)
        self.provider.assert_not_called(); self.broker.assert_not_called()
        before = self.item()
        self.assertEqual(self.callback('status', CallStatus='completed')['statusCode'], 200)
        self.assertEqual(self.item(), before)
        self.final.assert_called_once()

    def test_number_status_hangup_before_connect_preserves_owner_stop(self):
        self.configure(); self.callback()
        r._table.put_item(Item={**m.VOICE_KEY, 'activeRequest':'owner-stop-2026-10-16'})
        self.provider.reset_mock()
        self.assertEqual(self.callback('status', CallStatus='completed')['statusCode'], 200)
        self.assertEqual(self.item()['inboundState'], 'ended')
        self.assertNotIn('consented', self.item())
        self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'], 'owner-stop-2026-10-16')
        self.provider.assert_not_called(); self.broker.assert_not_called()

    def test_number_status_preserves_unrelated_call_lock(self):
        self.configure(); self.callback()
        r._table.put_item(Item={**m.VOICE_KEY, 'activeRequest':'owner:another:request'})
        self.assertEqual(self.callback('status', CallStatus='failed')['statusCode'], 200)
        self.assertEqual(self.item()['providerStatus'], 'failed')
        self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'], 'owner:another:request')

    def test_number_status_rejects_untrusted_provider_and_call_identity(self):
        self.configure(); self.callback()
        before = self.item()
        self.assertEqual(i.callback('/inbound-owner/status', b'', {})['statusCode'], 403)
        for fields in ({'AccountSid':'AC'+'9'*32}, {'From':'+12025550999'}, {'To':'+12025550999'},
                       {'From':None}, {'To':None}, {'Direction':'outbound-api'}, {'Direction':None}, {'CallStatus':'unexpected'}):
            with self.subTest(fields=fields):
                self.assertEqual(self.callback('status', **{'CallStatus':'completed', **fields})['statusCode'], 403)
        self.canonical.get_item.return_value = {'Item':{'userId':owners.OTHER}}
        self.assertEqual(self.callback('status', CallStatus='completed')['statusCode'], 403)
        self.assertEqual(self.item(), before)
        self.final.assert_not_called()

    def test_number_status_unknown_deleted_and_nonterminal_calls_make_no_records(self):
        self.configure()
        before = dict(r._table.items)
        reply = self.callback('status', CallStatus='completed')
        self.assertEqual(reply['statusCode'], 200); self.assertEqual(reply['body'], '{}')
        self.assertEqual(r._table.items, before)
        self.callback(); before = self.item()
        self.assertEqual(self.callback('status', CallStatus='ringing')['statusCode'], 200)
        self.assertEqual(self.item(), before)
        r._table.put_item(Item={'PK':'OWNER#'+owners.OWNER, 'SK':'ACCOUNT_DELETION', 'ownerUserId':owners.OWNER})
        before = dict(r._table.items)
        self.assertEqual(self.callback('status', CallStatus='completed')['statusCode'], 200)
        self.assertEqual(r._table.items, before)
        before_item = self.item()
        r._table.items.pop((before_item['PK'], before_item['SK']))
        remaining = dict(r._table.items)
        self.assertEqual(self.callback('status', CallStatus='completed')['statusCode'], 200)
        self.assertEqual(r._table.items, remaining)
        self.final.assert_not_called()

    def test_ambiguous_mint_never_retries(self):
        self.configure(); self.callback()
        self.broker.side_effect = TimeoutError()
        self.assertIn('<Hangup', self.callback('connect')['body'])
        self.callback('connect')
        self.broker.assert_called_once()
        self.assertEqual(self.item()['inboundState'], 'ended')

    def test_quota_refusal_is_spoken_before_the_signed_end(self):
        self.configure(); self.callback()
        provider = Mock()
        refusal = {'error': 'quota_exceeded', 'code': 402, 'kind': 'monthly_tokens', 'resetAt': '2026-11-01T00:00:00Z'}
        provider.invoke.return_value = {'Payload': io.BytesIO(json.dumps(refusal).encode())}
        with patch.dict(sys.modules, {'boto3': types.SimpleNamespace(client=Mock(return_value=provider))}):
            with self.assertRaises(i.QuotaRefused) as raised:
                REAL_MINT(self.item())
        self.broker.side_effect = raised.exception
        reply = self.callback('connect')['body']
        self.assertIn('Your Live Ninja monthly usage limit is reached, so this call cannot start. It resets on November 1. Goodbye.', reply)
        self.assertIn('ended/' + self.item()['callbackId'] + '</Redirect>', reply)
        # The call stays up for the spoken reason: no provider hangup, no recording, lock held.
        self.assertFalse(any((call.args[1:] or [{}])[0] == {'Status': 'completed'} for call in self.provider.call_args_list))
        self.assertFalse(any('/Recordings' in call.args[0] for call in self.provider.call_args_list))
        item = self.item()
        self.assertEqual((item['inboundState'], item['refusalReason']), ('refused', 'quota_monthly_tokens'))
        self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'], item['lockId'])
        self.assertNotIn('<Connect', self.callback('connect')['body'])
        self.broker.assert_called_once()
        self.callback('ended', CallStatus='in-progress')
        self.assertEqual((self.item()['inboundState'], self.item()['endReason']), ('ended', 'quota_monthly_tokens'))
        self.assertNotEqual((r.get(m.VOICE_KEY) or {}).get('activeRequest'), item['lockId'])
        self.assertEqual(i.QuotaRefused('rate_limited', None).speech(), 'Your Live Ninja usage limit is reached, so this call cannot start. Goodbye.')

    def test_quota_refusal_keeps_reason_when_caller_hangs_up_during_message(self):
        self.configure(); self.callback()
        self.broker.side_effect = i.QuotaRefused('monthly_tokens', '2026-11-01T00:00:00Z')
        self.assertIn('monthly usage limit', self.callback('connect')['body'])
        self.callback('status', CallStatus='completed')
        self.assertEqual((self.item()['inboundState'], self.item()['endReason']), ('ended', 'quota_monthly_tokens'))
        self.assertNotEqual((r.get(m.VOICE_KEY) or {}).get('activeRequest'), self.item()['lockId'])

    def test_other_broker_errors_keep_immediate_hangup(self):
        self.configure(); self.callback()
        provider = Mock()
        provider.invoke.return_value = {'Payload': io.BytesIO(json.dumps({'error': 'phone_unavailable', 'code': 503}).encode())}
        with patch.dict(sys.modules, {'boto3': types.SimpleNamespace(client=Mock(return_value=provider))}):
            with self.assertRaises(ValueError):
                REAL_MINT(self.item())
        self.broker.side_effect = ValueError('Invalid broker bootstrap')
        reply = self.callback('connect')['body']
        self.assertNotIn('<Redirect', reply)
        self.assertEqual((self.item()['inboundState'], self.item()['endReason']), ('ended', 'bootstrap_failed'))
        self.assertIn({'Status': 'completed'}, [(call.args[1:] or [{}])[0] for call in self.provider.call_args_list])

    def test_minted_session_finalized_when_bootstrap_persistence_loses_race(self):
        self.configure(); self.callback()
        transition = i.transition
        def lose_after_mint(item, **fields):
            if 'bootstrap' in fields:
                raise f.Conflict()
            return transition(item, **fields)
        with patch.object(i, 'transition', side_effect=lose_after_mint):
            self.assertIn('<Hangup', self.callback('connect')['body'])
        self.broker.assert_called_once()
        self.final.assert_called_once()
        self.assertEqual(self.final.call_args.args[0]['bootstrap']['sessionId'], 'fresh-session')
        self.assertEqual(self.final.call_args.args[0]['bootstrap']['bridgeToken'], 'test-capability')
        self.assertFalse(any('/Recordings' in call.args[0] for call in self.provider.call_args_list))

    def test_failed_provider_hangup_still_finalizes_session_without_releasing_lock(self):
        self.configure(); self.callback(); self.callback('connect')
        item = self.item()
        self.provider.side_effect = m.ProviderError()
        self.assertFalse(i.end(item, 'bridge_failed', hangup=True))
        self.final.assert_called_once()
        self.assertEqual(self.final.call_args.args[0]['bootstrap']['sessionId'], 'fresh-session')
        self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'], item['lockId'])
        self.assertNotEqual(self.item()['providerStatus'], 'completed')
        self.assertTrue(self.item()['transcriptFinalized'])
        self.assertIn('provider_cleanup_unconfirmed', self.item()['endReason'])
        # A later cleanup attempt must not finalize the already-ended broker
        # session again, and a mismatched provider reply cannot release the lock.
        self.provider.side_effect = None
        self.provider.return_value = {'sid': 'CA'+'9'*32, 'account_sid': f.ACCOUNT, 'status': 'completed'}
        self.assertFalse(i.end(self.item(), 'bridge_failed', hangup=True))
        self.final.assert_called_once()
        self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'], item['lockId'])

    def test_duplicate_connect_callbacks_mint_and_record_once(self):
        self.configure(); self.callback()
        reservations = r.get(m.budget_keys(*m.budget_clock(r.get(m.CONFIG_KEY))[:2])[0])
        self.callback()
        self.assertEqual(r.get(m.budget_keys(*m.budget_clock(r.get(m.CONFIG_KEY))[:2])[0]), reservations)
        reply = self.callback('connect')
        self.assertIn('<Connect', reply['body'])
        self.assertNotIn('<Say', reply['body'])
        self.assertEqual(self.item()['recordingAuthorization'], 'owner-standing')
        call = r.get(r.call_key(f.CALL, m.namespace(owners.OWNER)))
        self.assertEqual(call['recordingAuthorization'], 'owner-standing')
        self.assertIn('<Hangup', self.callback('connect')['body'])
        self.broker.assert_called_once()
        self.assertEqual(sum('/Recordings' in call.args[0] for call in self.provider.call_args_list), 1)

    def test_expired_connect_and_session_deadline_never_mint(self):
        self.configure(); self.callback()
        self.now.return_value = 1030
        self.assertIn('<Hangup', self.callback('connect')['body'])
        self.assertNotIn('pinAttempts', self.item())
        self.now.return_value = 1600
        self.assertIn('<Hangup', self.callback('connect')['body'])
        self.broker.assert_not_called()

    def test_invalid_account_destination_and_caller_do_not_advance_connect(self):
        self.configure(); self.callback()
        for fields in ({'AccountSid': 'AC'+'9'*32}, {'From': '+12025550999'}, {'To': '+12025550999'}):
            self.assertEqual(self.callback('connect', **fields)['statusCode'], 403)
        self.assertNotIn('pinAttempts', self.item())

    def test_terminal_failure_is_not_reported_as_completed_and_unbound_stop_keeps_lock(self):
        self.configure(); self.callback()
        self.provider.side_effect = lambda *args: {'sid': 'CA'+'9'*32, 'account_sid': f.ACCOUNT, 'status': 'completed'}
        self.assertFalse(i.end(self.item(), 'test_failure', hangup=True))
        self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'], self.item()['lockId'])
        self.callback('status', CallStatus='failed')
        self.assertEqual(self.item()['providerStatus'], 'failed')
        self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))

    def test_finished_bridge_does_not_repeat_transcript_final_on_terminal_callback(self):
        self.configure(); self.callback(); self.callback('connect')
        self.control()
        raw = json.dumps({'capability': self.item()['callbackId'], 'callSid': f.CALL,
                          'transcriptFinalized': True, 'reason': 'remote_hangup', 'inputTokens': 123, 'outputTokens': 45,
                          'modelCostUSD': '0.123456'}).encode()
        self.assertEqual(i.redeem(raw, {'x-ln-signature': r.digest(b'inbound-owner/finish\n' + raw)}, 'finish')['statusCode'], 200)
        self.callback('status', CallStatus='completed')
        self.final.assert_not_called()
        self.assertTrue(self.item()['transcriptFinalized'])
        self.assertEqual(self.item()['endReason'], 'remote_hangup')
        self.assertEqual(self.item()['inputTokens'], 123)
        self.assertEqual(self.item()['modelCostUSD'], Decimal('0.123456'))

    def finish(self, sid=f.CALL, **fields):
        item = r.get(m.request_key(owners.OWNER, 'inbound-' + sid))
        raw = json.dumps({'capability': item['callbackId'], 'callSid': sid, 'transcriptFinalized': True,
                          'reason': 'remote_hangup', **fields}).encode()
        return i.redeem(raw, {'x-ln-signature': r.digest(b'inbound-owner/finish\n' + raw)}, 'finish')

    def test_cost_cap_is_ten_dollars_less_the_telephone_reserve_for_a_fresh_day(self):
        self.configure(); self.callback()
        item = self.item()
        self.assertEqual(m.TELEPHONE_RESERVE, Decimal('0.459'), '10 started minutes at 0.0209 plus the 0.25 recording charge')
        self.assertEqual((item['costCapUSD'], item['telephoneReserveUSD'], item['reservedUSD'], item['maxCallSeconds']),
                         (Decimal('9.541'), Decimal('0.459'), Decimal('10'), 600))
        self.assertEqual(item['deadline'], 1600)
        self.assertEqual(item['connectDeadline'], 1030, 'connectDeadline is unchanged')
        month, day = m.budget_clock(r.get(m.CONFIG_KEY))[:2]
        self.assertEqual(r.get(m.budget_keys(month, day)[1])['reservedUSD'], Decimal('10'))
        self.assertEqual(r.get(m.budget_keys(month, day)[0])['unsettled'], {'inbound-' + f.CALL: Decimal('10')})

    def test_cost_cap_shrinks_to_the_remaining_daily_allowance(self):
        self.configure()
        month, day, zone, expires = m.budget_clock(r.get(m.CONFIG_KEY))
        r._table.put_item(Item={**m.budget_keys(month, day)[1], 'version': 1, 'reservedUSD': Decimal('7'), 'expiresAt': expires})
        self.assertIn('<Redirect', self.callback()['body'])
        self.assertEqual((self.item()['costCapUSD'], self.item()['reservedUSD']), (Decimal('2.541'), Decimal('3')))
        self.assertEqual(r.get(m.budget_keys(month, day)[1])['reservedUSD'], Decimal('10'))
        self.assertIn('<Connect', self.callback('connect')['body'])
        self.assertEqual(json.loads(self.control()['body'])['costCapUSD'], '2.541000')

    def test_cost_cap_shrinks_to_the_remaining_monthly_allowance(self):
        self.configure()
        month = m.budget_clock(r.get(m.CONFIG_KEY))[0]
        r._table.put_item(Item={**m.budget_keys(month, '')[0], 'version': 2, 'allocatedUSD': Decimal('95'),
                                'ledgerVersion': m.LEDGER_VERSION, 'unsettled': {}})
        self.assertIn('<Redirect', self.callback()['body'])
        self.assertEqual((self.item()['costCapUSD'], self.item()['reservedUSD']), (Decimal('4.541'), Decimal('5')))
        self.assertEqual(r.get(m.budget_keys(month, '')[0])['allocatedUSD'], Decimal('100'))

    def test_cost_cap_at_or_below_zero_refuses_with_the_binding_reason(self):
        self.configure()
        month, day, zone, expires = m.budget_clock(r.get(m.CONFIG_KEY))
        day_key = m.budget_keys(month, day)[1]
        for reserved in (Decimal('9.541'), Decimal('9.60'), Decimal('10')):
            with self.subTest(reserved=reserved):
                r._table.put_item(Item={**day_key, 'version': 1, 'reservedUSD': reserved, 'expiresAt': expires})
                body = self.callback()['body']
                self.assertIn("Today's call allowance is used. Please call again tomorrow.", body)
                self.assertIn('<Hangup', body)
                self.assertIsNone(self.item()); self.assertIsNone(r.get(m.VOICE_KEY))
                self.assertEqual(r.get(day_key)['reservedUSD'], reserved)
        r._table.items.pop((day_key['PK'], day_key['SK']))
        r._table.put_item(Item={**m.budget_keys(month, '')[0], 'version': 2, 'allocatedUSD': Decimal('99.541'),
                                'ledgerVersion': m.LEDGER_VERSION, 'unsettled': {}})
        body = self.callback()['body']
        self.assertIn('The monthly call allowance is used. Calls resume next month.', body)
        self.assertIsNone(self.item()); self.assertIsNone(r.get(day_key))
        self.broker.assert_not_called()
        self.assertFalse(any(call.args[0] == '/Calls.json' for call in self.provider.call_args_list))

    def test_bootstrap_without_a_valid_cost_cap_is_not_redeemable(self):
        self.configure(); self.callback(); self.callback('connect')
        stored = self.item()
        for bad in (None, Decimal('0'), Decimal('-1'), Decimal('10.01'), '9.5'):
            with self.subTest(cap=bad):
                item = dict(stored)
                if bad is None:
                    item.pop('costCapUSD')
                else:
                    item['costCapUSD'] = bad
                r._table.put_item(Item=item)
                self.assertEqual(self.control()['statusCode'], 403)
                self.assertEqual(self.item()['inboundState'], 'ready')
        r._table.put_item(Item=stored)
        self.assertEqual(self.control()['statusCode'], 200)

    def test_finish_model_cost_must_be_a_six_place_decimal_string(self):
        self.configure(); self.callback(); self.callback('connect'); self.control()
        for bad in ('0.1', '1', 0.25, '-0.000001', 'NaN', '12345.000000', 'abc'):
            with self.subTest(value=bad):
                self.assertEqual(self.finish(inputTokens=1, outputTokens=1, modelCostUSD=bad)['statusCode'], 400)
                self.assertEqual(self.item()['inboundState'], 'redeemed')
                self.assertNotIn('modelCostUSD', self.item())
        self.assertEqual(self.finish(inputTokens=1, outputTokens=1, modelCostUSD='0.000000')['statusCode'], 200)
        self.assertEqual(self.item()['modelCostUSD'], Decimal('0'))

    def settling_provider(self, price=None, duration='125'):
        original = self.provider_result
        def lookup(path, form=None):
            if path == '/Calls/' + f.CALL + '.json' and form is None:
                result = {'sid': f.CALL, 'account_sid': f.ACCOUNT, 'status': 'completed', 'duration': duration}
                if price is not None:
                    result.update(price=price, price_unit='USD')
                return result
            return original(path, form)
        self.provider.side_effect = lookup

    def test_settlement_adds_the_bridge_model_cost_to_the_telephone_estimate(self):
        self.configure(); self.callback(); self.callback('connect'); self.control()
        self.assertEqual(self.finish(inputTokens=5000, outputTokens=900, modelCostUSD='1.234567')['statusCode'], 200)
        self.settling_provider(price='-0.0425', duration='125')
        cfg = r.get(m.CONFIG_KEY)
        month, day = m.budget_clock(cfg)[:2]
        month_key, day_key = m.budget_keys(month, day)
        status = m.budget_status(cfg)
        # 125 s -> 3 started minutes: 3 * 0.0209 + 0.25 = 0.3127 (above the provider's 0.0425), plus 1.234567.
        self.assertEqual(self.item()['settledUSD'], Decimal('1.547267'))
        self.assertEqual(r.get(day_key)['reservedUSD'], Decimal('1.547267'))
        self.assertEqual((Decimal(status['allocatedUSD']), status['unsettledCalls']), (Decimal('9.547267'), 0))

    def test_settlement_without_a_model_cost_keeps_the_reservation(self):
        self.configure(); self.callback(); self.callback('connect'); self.control()
        self.assertEqual(self.finish(inputTokens=5000, outputTokens=900)['statusCode'], 200)
        self.settling_provider(price='-0.0425', duration='125')
        cfg = r.get(m.CONFIG_KEY)
        month, day = m.budget_clock(cfg)[:2]
        month_key, day_key = m.budget_keys(month, day)
        status = m.budget_status(cfg)
        self.assertNotIn('settledUSD', self.item())
        self.assertEqual(r.get(day_key)['reservedUSD'], Decimal('10'), 'unknown model cost keeps the reservation')
        self.assertEqual((Decimal(status['allocatedUSD']), status['unsettledCalls']), (Decimal('18'), 1))
        self.assertEqual(self.item()['inputTokens'], 5000, 'token counts alone never price an owner conversation')

    def test_finish_accepts_the_token_counts_of_a_ten_minute_call(self):
        # A 600 s owner call reports well over the old 100000-token bound (the bridge
        # measures about 156000 input tokens for a normal ten-minute call).
        self.configure(); self.callback(); self.callback('connect'); self.control()
        self.assertEqual(self.finish(inputTokens=156000, outputTokens=9000, modelCostUSD='1.900000')['statusCode'], 200)
        self.assertEqual(self.item()['inputTokens'], 156000)
        self.assertEqual(self.finish(inputTokens=10_000_001, outputTokens=1)['statusCode'], 400, 'the bound still exists')

    def rejections(self):
        return [row['data'] for row in r._table.items.values() if row.get('type') == 'inbound_usage_rejected']

    def test_call_that_never_reached_the_bridge_settles_at_the_telephone_estimate(self):
        self.configure(); self.callback()
        def reject_cap(path, form=None):
            if form and 'TimeLimit' in form:
                raise m.ProviderError(21220, rejected=True)
            return self.provider_result(path, form)
        self.provider.side_effect = reject_cap
        self.assertIn('<Hangup', self.callback('connect')['body'])
        self.assertEqual(self.item()['endReason'], 'provider_cap_failed_21220')
        self.assertNotIn('redeemedAt', self.item())
        self.broker.assert_not_called()
        self.settling_provider(price='-0.0071', duration='20')
        cfg = r.get(m.CONFIG_KEY)
        month, day = m.budget_clock(cfg)[:2]
        month_key, day_key = m.budget_keys(month, day)
        status = m.budget_status(cfg)
        # 20 s -> one started minute: 0.0209 + 0.25 = 0.2709; the bridge never ran a model turn.
        self.assertEqual(self.item()['settledUSD'], Decimal('0.2709'))
        self.assertEqual(r.get(day_key)['reservedUSD'], Decimal('0.2709'))
        self.assertEqual((Decimal(status['allocatedUSD']), status['unsettledCalls']), (Decimal('8.2709'), 0))
        self.assertEqual(status['pausedReason'], '')
        # A model cost reported after that settlement is refused and recorded; the ledger is closed.
        self.assertEqual(self.finish(inputTokens=1, outputTokens=1, modelCostUSD='0.500000')['statusCode'], 409)
        self.assertNotIn('modelCostUSD', self.item())
        self.assertEqual(self.rejections(), [{'reason': 'call_already_settled', 'reportedModelCostUSD': '0.500000'}])
        self.assertEqual(self.item()['settledUSD'], Decimal('0.2709'))

    def test_redeemed_call_without_a_finish_keeps_its_reservation(self):
        self.configure(); self.callback(); self.callback('connect'); self.control()
        self.assertEqual(self.item()['inboundState'], 'redeemed')
        self.assertIsInstance(self.item()['redeemedAt'], int)
        # The provider reports the call over before any bridge finish arrives (bridge crash).
        self.callback('status', CallStatus='completed')
        self.settling_provider(price='-0.0425', duration='125')
        cfg = r.get(m.CONFIG_KEY)
        month, day = m.budget_clock(cfg)[:2]
        m.budget_status(cfg)
        self.assertNotIn('settledUSD', self.item())
        self.assertEqual(r.get(m.budget_keys(month, day)[1])['reservedUSD'], Decimal('10'), 'a redeemed call has an unknown model cost until the bridge reports it')

    def test_first_reported_model_cost_is_authoritative(self):
        self.configure(); self.callback(); self.callback('connect'); self.control()
        self.assertEqual(self.finish(inputTokens=10, outputTokens=10, modelCostUSD='0.250000')['statusCode'], 200)
        self.assertEqual(self.finish(inputTokens=10, outputTokens=10, modelCostUSD='0.250000')['statusCode'], 200, 'an identical repeat is idempotent')
        self.assertEqual(self.finish(inputTokens=10, outputTokens=10, modelCostUSD='0.300000')['statusCode'], 409)
        self.assertEqual(self.item()['modelCostUSD'], Decimal('0.25'))
        self.assertEqual(self.rejections(), [{'reason': 'model_cost_already_reported', 'reportedModelCostUSD': '0.300000'}])

    def test_model_cost_above_the_per_call_cap_is_refused_and_above_the_call_cap_is_flagged(self):
        self.configure(); self.callback(); self.callback('connect'); self.control()
        self.assertEqual(self.item()['costCapUSD'], Decimal('9.541'))
        for above in ('10.000001', '12.345678', '9999.999999'):
            with self.subTest(value=above):
                self.assertEqual(self.finish(inputTokens=1, outputTokens=1, modelCostUSD=above)['statusCode'], 400)
                self.assertNotIn('modelCostUSD', self.item())
                self.assertEqual(self.item()['inboundState'], 'redeemed')
        self.assertEqual([row['reason'] for row in self.rejections()], ['invalid_model_cost'] * 3)
        self.assertEqual(sorted(row['reportedModelCostUSD'] for row in self.rejections()), ['10.000001', '12.345678', '9999.999999'])
        # Within the per-call cap but above this call's own cap: stored, and the overage is visible.
        self.assertEqual(self.finish(inputTokens=1, outputTokens=1, modelCostUSD='9.600000')['statusCode'], 200)
        self.assertEqual((self.item()['modelCostUSD'], self.item()['costCapExceeded']), (Decimal('9.6'), True))
        usage = [row['data'] for row in r._table.items.values() if row.get('type') == 'inbound_usage']
        self.assertEqual(usage, [{'inputTokens': 1, 'outputTokens': 1, 'modelCostUSD': '9.600000', 'costCapExceeded': True, 'costCapUSD': '9.541000'}])

    def test_settlement_never_drops_below_the_provider_measured_price(self):
        self.configure(); self.callback(); self.callback('connect'); self.control()
        self.assertEqual(self.finish(inputTokens=10, outputTokens=10, modelCostUSD='0.010000')['statusCode'], 200)
        self.settling_provider(price='-0.90', duration='30')
        cfg = r.get(m.CONFIG_KEY)
        month, day = m.budget_clock(cfg)[:2]
        m.budget_status(cfg)
        # The provider's 0.90 is above the 0.2709 one-minute estimate and is kept.
        self.assertEqual(self.item()['settledUSD'], Decimal('0.91'))
        self.assertEqual(r.get(m.budget_keys(month, day)[1])['reservedUSD'], Decimal('0.91'))

    def test_fresh_account_revocation_blocks_media_heartbeat(self):
        self.configure(); self.callback(); self.callback('connect')
        self.control()
        self.owner_active.return_value = False
        self.assertEqual(self.control('check')['statusCode'], 403)
        self.owner_active.assert_called_with(owners.OWNER, since=1000)

    def test_control_signature_cannot_be_replayed_across_actions(self):
        self.configure(); self.callback(); self.callback('connect')
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
        self.configure(); self.callback(); self.callback('connect')
        stored = self.item()
        stored['deadline'] = Decimal('1600')
        r._table.put_item(Item=stored)
        reply = self.control()
        self.assertEqual(reply['statusCode'], 200)
        bootstrap = json.loads(reply['body'])
        deadline = bootstrap['deadline']
        self.assertIs(type(deadline), int)
        self.assertEqual(deadline, 1600)
        self.assertEqual(bootstrap['costCapUSD'], '9.541000', 'a decimal string, never a float')
        numeric = json.loads(r.response(200, {'integer': Decimal('42.000'), 'fraction': Decimal('1.40')})['body'])
        self.assertIs(type(numeric['integer']), int)
        self.assertEqual(numeric['fraction'], 1.4)

    def test_recording_timeout_keeps_attempted_intent_for_late_cleanup(self):
        self.configure(); self.callback()
        def timeout(path, form=None):
            if path.endswith('/Recordings.json'):
                self.assertEqual(r.get(r.call_key(f.CALL, m.namespace(owners.OWNER)))['recordingCreateState'], 'attempted')
                raise m.ProviderError()
            return self.provider_result(path, form)
        self.provider.side_effect = timeout
        self.callback('connect')
        call = r.get(r.call_key(f.CALL, m.namespace(owners.OWNER)))
        self.assertEqual(call['recordingCreateState'], 'attempted')
        self.assertEqual(call['recordingStatus'], 'failed')
        self.assertEqual(call['recordingReason'], 'recording_create_unknown')
        r._table.put_item(Item={'PK': 'OWNER#'+owners.OWNER, 'SK': 'ACCOUNT_DELETION', 'ownerUserId': owners.OWNER})
        with patch('account_lifecycle.resolve_deleted_recording', return_value=r.response(200)) as resolve, patch.object(r, 'copy_audio') as copy_audio:
            self.assertEqual(self.callback('recording', RecordingSid=f.REC, RecordingStatus='completed', From='+12025550999')['statusCode'], 403)
            resolve.assert_not_called()
            self.assertEqual(self.callback('recording', RecordingSid=f.REC, RecordingStatus='completed')['statusCode'], 200)
            self.assertEqual(resolve.call_args.args, (call, f.REC, 'completed'))
            copy_audio.assert_not_called()

    def test_known_recording_rejection_is_not_an_ambiguous_creation(self):
        self.configure(); self.callback()
        def rejected(path, form=None):
            if path.endswith('/Recordings.json'):
                raise m.ProviderError(21220, rejected=True)
            return self.provider_result(path, form)
        self.provider.side_effect = rejected
        self.callback('connect')
        call = r.get(r.call_key(f.CALL, m.namespace(owners.OWNER)))
        self.assertEqual(call['recordingCreateState'], 'rejected')
        self.assertEqual(call['recordingStatus'], 'absent')

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
            'surface': 'phone', 'phoneCallSid': f.CALL, 'phoneDeadline': 1600})
        provider.invoke.return_value = {'Payload': io.BytesIO(json.dumps(bootstrap).encode())}
        with patch.dict(sys.modules, {'boto3': fake_boto}):
            REAL_MINT({**self.item(), 'live':True, 'message':'Sales update', 'payload':'{"total":12}'})
        wire=json.loads(provider.invoke.call_args.kwargs['Payload'])
        self.assertEqual(wire['phoneSummary'],'Sales update')
        self.assertEqual(wire['phonePayload'],'{"total":12}')

    def test_outbound_terminal_before_create_response_stays_terminal(self):
        self.configure()
        self.item=lambda:r.get(m.request_key(owners.OWNER,'early-live'))
        def early(path, form=None):
            if path == '/Calls.json':
                self.assertIn('<Connect',self.callback('connect',From='+12025550100',To='+12025550101',Direction='outbound-api')['body'])
                self.assertEqual(self.control()['statusCode'],200)
                self.assertEqual(self.control('finish')['statusCode'],200)
                return {'sid':f.CALL,'account_sid':f.ACCOUNT,'status':'queued'}
            return self.provider_result(path,form)
        self.provider.side_effect=early
        result=r.handler({'operation':'owner_message_send','ownerUserId':owners.OWNER,'requestId':'early-live',
                          'destination':'primary','message':'Summary','live':True},None)
        self.assertEqual(result['statusCode'],200)
        self.assertEqual(self.item()['providerStatus'],'completed')
        self.assertEqual(self.item()['inboundState'],'ended')
        self.assertNotIn('activeRequest',r.get(m.VOICE_KEY))


if __name__ == '__main__':
    unittest.main()
