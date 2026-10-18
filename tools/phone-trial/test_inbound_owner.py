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
        cfg = r.get(m.CONFIG_KEY); cfg['budgetFloorUSD'] = 20
        cfg['budgetFloorMonth'] = m.budget_clock(cfg)[0]; r._table.put_item(Item=cfg)
        self.assertIn('<Hangup', self.callback()['body'])
        self.assertIsNone(self.item())
        self.assertEqual(self.provider.call_count, 1)
        self.broker.assert_not_called()

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

    def test_both_configured_owner_aliases_require_only_recording_consent(self):
        for caller in ('+12025550101','+12025550102'):
            with self.subTest(caller=caller):
                r._table=f.Table();self.configure()
                self.assertNotIn('pinHash',r.get(i.CONFIG));self.assertNotIn('pinSalt',r.get(i.CONFIG))
                first=self.callback(From=caller)
                self.assertIn('numDigits="1"',first['body']);self.assertIn('Press 1',first['body']);self.assertNotIn('PIN',first['body'])
                self.assertEqual(self.item()['inboundState'],'consent')
                self.assertEqual(self.item()['consentDeadline'],1030)
                self.assertNotIn('pinAttempts',self.item())
                self.broker.assert_not_called()
                self.assertIn('<Connect',self.callback('consent',From=caller,Digits='1')['body'])
                self.broker.assert_called_once()

    def test_old_pin_routes_are_removed_and_unsigned_callbacks_rejected(self):
        self.configure();self.callback()
        before=self.item()
        self.assertEqual(self.callback('pin/1',Digits='123456')['statusCode'],404)
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
        self.assertIn('Press 1',self.callback()['body'])

    def test_caller_revoked_after_gather_cannot_mint_owner_session(self):
        self.configure();self.callback()
        cfg=r.get(m.CONFIG_KEY);cfg['destinations']['primary']='+12025550999';r._table.put_item(Item=cfg)
        self.assertIn('<Hangup',self.callback('consent',Digits='1')['body'])
        self.assertEqual(self.item()['endReason'],'admission_revoked')
        self.broker.assert_not_called()
        self.assertFalse(any('/Recordings' in call.args[0] for call in self.provider.call_args_list))

    def test_caller_revocation_blocks_active_media_heartbeat(self):
        self.configure();self.callback();self.callback('consent',Digits='1');self.control()
        cfg=r.get(m.CONFIG_KEY);cfg['destinations']['primary']='+12025550999';r._table.put_item(Item=cfg)
        self.assertEqual(self.control('check')['statusCode'],403)

    def test_caller_consent_and_single_use_bootstrap_preserve_identity_and_recording(self):
        self.configure()
        self.assertIn('Press 1', self.callback()['body'])
        self.assertFalse(any(call.args[1] for call in self.provider.call_args_list if len(call.args)>1))
        self.broker.assert_not_called()
        reply = self.callback('consent', Digits='1')
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
        self.assertIn('Press 1', initial['body'])
        self.assertIn('numDigits="1"', initial['body'])
        self.assertIn('timeout="6"', initial['body'])
        self.assertIn('actionOnEmptyResult="true"', initial['body'])
        self.assertIn('<Hangup/>', initial['body'])
        self.assertNotIn('PIN', initial['body'])
        self.assertEqual(caps, [])
        answered = True
        self.now.return_value = 1005
        self.assertIn('<Connect', self.callback('consent', Digits='1')['body'])
        self.assertEqual(caps, [175])
        self.now.return_value = 1010
        self.assertIn('<Hangup', self.callback('consent', Digits='1')['body'])
        self.assertEqual(caps, [175])
        self.assertEqual(self.item()['deadline'], 1180)
        self.broker.assert_called_once()

    def test_consent_cap_rejection_stops_before_mint_and_records_only_code(self):
        self.configure(); self.callback()
        def reject_cap(path, form=None):
            if form and 'TimeLimit' in form:
                error = m.ProviderError(21220, rejected=True)
                error.args = ('RAW_PROVIDER_RESPONSE_MUST_NOT_BE_STORED',)
                raise error
            return self.provider_result(path, form)
        self.provider.side_effect = reject_cap
        self.assertIn('<Hangup', self.callback('consent', Digits='1')['body'])
        self.assertEqual(self.item()['inboundState'], 'ended')
        self.assertEqual(self.item()['endReason'], 'provider_cap_failed_21220')
        self.assertNotIn('providerCapSet', self.item())
        self.assertTrue(self.item()['consented'])
        self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))
        self.broker.assert_not_called()
        self.assertFalse(any('/Recordings' in call.args[0] for call in self.provider.call_args_list))
        self.assertNotIn('RAW_PROVIDER_RESPONSE', json.dumps(list(r._table.items.values()), default=str))

    def test_consent_cap_response_identity_and_state_fail_closed(self):
        for bad in ({'sid':'CA'+'9'*32}, {'account_sid':'AC'+'9'*32}, {'status':'ringing'}):
            with self.subTest(response=bad):
                r._table = f.Table()
                self.configure(); self.callback()
                def bad_cap(path, form=None):
                    result = self.provider_result(path, form)
                    return {**result, **bad} if form and 'TimeLimit' in form else result
                self.provider.side_effect = bad_cap
                self.assertIn('<Hangup', self.callback('consent', Digits='1')['body'])
                self.assertEqual(self.item()['endReason'], 'provider_cap_failed')
                self.assertNotIn('providerCapSet', self.item())
                self.assertTrue(self.item()['consented'])
                self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))
                self.broker.assert_not_called()

    def test_recurring_incoming_allowance_survives_monthly_limit_without_generic_grant(self):
        self.configure_daily()
        self.assertIn('<Gather', self.callback()['body'])
        self.assertEqual(self.incoming_day()['reservedUSD'], Decimal('1.40'))
        self.assertEqual(self.item()['budgetExpiresAt'], 2000)
        self.assertEqual(self.item()['budgetSource'], 'incoming-daily-policy')
        self.assertIsNone(r.get(m.budget_keys('2026-10', '2026-10-18')[1]))
        self.assertEqual(r.get(m.budget_keys('2026-10', '2026-10-18')[0])['allocatedUSD'], Decimal('21.40'))
        self.callback('status', CallStatus='completed')
        self.callback()  # Same CallSid cannot reserve again even after releasing its voice lock.
        self.assertEqual(self.incoming_day()['reservedUSD'], Decimal('1.40'))
        status = json.loads(r.handler({'operation':'owner_budget_status','ownerUserId':owners.OWNER}, None)['body'])
        self.assertEqual(status['dailyGrantedUSD'], '0')
        self.assertEqual(status['pausedReason'], 'waiting_approval')
        self.assertEqual(status['incomingDailyBudget'], {'enabled': True, 'limitUSD':'10', 'reservedUSD':'1.40',
            'remainingUSD':'8.60', 'day':'2026-10-18', 'timezone':'America/New_York', 'expiresAt':2000})
        self.assertNotIn('callerNumber', json.dumps(status)); self.assertNotIn('+12025550101', json.dumps(status))
        self.assertNotIn('pinHash', json.dumps(status)); self.assertNotIn('pinSalt', json.dumps(status))

    def test_recurring_allowance_does_not_fund_other_caller_outbound_or_hotel(self):
        self.configure_daily()
        self.assertIn('<Hangup', self.callback(From='+12025550102')['body'])
        self.assertIsNone(self.incoming_day())
        outgoing = r.handler({'operation':'owner_message_send','ownerUserId':owners.OWNER,'requestId':'outbound-policy-test',
                              'destination':'primary','message':'Test'}, None)
        self.assertEqual(outgoing['statusCode'], 409)
        cfg = r.get(m.CONFIG_KEY)
        with self.assertRaisesRegex(ValueError, 'waiting_approval'):
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
        def dynamic_call(path, form=None):
            if path.startswith('/Calls/'):
                return {'sid':path.split('/')[2].split('.')[0], 'account_sid':f.ACCOUNT,
                        'status':'completed' if (form or {}).get('Status') == 'completed' else 'in-progress'}
            return original(path, form)
        self.provider.side_effect = dynamic_call
        for number in range(1, 8):
            sid = 'CA' + format(number, '032x')
            self.assertIn('<Gather', self.callback(CallSid=sid)['body'])
            self.callback('status', CallSid=sid, CallStatus='completed')
        self.assertEqual(self.incoming_day()['reservedUSD'], Decimal('9.80'))
        exhausted = self.callback(CallSid='CA'+format(8,'032x'))
        self.assertIn('Please call again tomorrow', exhausted['body'])
        self.assertEqual(self.incoming_day()['reservedUSD'], Decimal('9.80'))
        self.assertEqual(r.get(manual_key), manual)
        self.assertNotIn('activeRequest', r.get(m.VOICE_KEY))
        self.budget_clock.return_value = ('2026-10', '2026-10-19', 'America/New_York', 4000)
        self.now.return_value = 3000
        self.assertIn('<Gather', self.callback(CallSid='CA'+format(9,'032x'))['body'])
        self.assertEqual(self.incoming_day('2026-10-19')['reservedUSD'], Decimal('1.40'))
        self.assertEqual(self.incoming_day()['reservedUSD'], Decimal('9.80'))

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
        self.assertIn('<Gather',self.callback()['body'])
        self.assertEqual(self.incoming_day()['reservedUSD'],Decimal('1.40'))
        self.assertEqual(self.item()['budgetExpiresAt'],2000)
        self.assertEqual(r.get(m.budget_keys('2026-10','2026-10-18')[0])['allocatedUSD'],Decimal('9.40'))
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
        self.assertIn('<Gather',self.callback()['body'])
        self.assertEqual(self.incoming_day('2026-11-01')['reservedUSD'],Decimal('1.40'))
        self.assertEqual(self.item()['inboundBudgetDay'],'2026-11-01')
        self.assertIsNone(r.get(m.budget_keys('2026-11','2026-11-01')[1]))

    def test_number_status_early_hangup_before_consent_releases_only_own_lock(self):
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

    def test_number_status_hangup_before_consent_preserves_owner_stop(self):
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
        self.assertIn('<Hangup', self.callback('consent', Digits='1')['body'])
        self.callback('consent', Digits='1')
        self.broker.assert_called_once()
        self.assertEqual(self.item()['inboundState'], 'ended')

    def test_minted_session_finalized_when_bootstrap_persistence_loses_race(self):
        self.configure(); self.callback()
        transition = i.transition
        def lose_after_mint(item, **fields):
            if 'bootstrap' in fields:
                raise f.Conflict()
            return transition(item, **fields)
        with patch.object(i, 'transition', side_effect=lose_after_mint):
            self.assertIn('<Hangup', self.callback('consent', Digits='1')['body'])
        self.broker.assert_called_once()
        self.final.assert_called_once()
        self.assertEqual(self.final.call_args.args[0]['bootstrap']['sessionId'], 'fresh-session')
        self.assertEqual(self.final.call_args.args[0]['bootstrap']['bridgeToken'], 'test-capability')
        self.assertFalse(any('/Recordings' in call.args[0] for call in self.provider.call_args_list))

    def test_failed_provider_hangup_still_finalizes_session_without_releasing_lock(self):
        self.configure(); self.callback(); self.callback('consent', Digits='1')
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

    def test_consent_refusal_and_duplicate_callbacks_do_not_record(self):
        self.configure(); self.callback()
        reservations = r.get(m.budget_keys(*m.budget_clock(r.get(m.CONFIG_KEY))[:2])[0])
        self.callback()
        self.assertEqual(r.get(m.budget_keys(*m.budget_clock(r.get(m.CONFIG_KEY))[:2])[0]), reservations)
        self.assertIn('<Hangup', self.callback('consent', Digits='2')['body'])
        self.callback('consent', Digits='1')
        self.assertEqual(self.item()['endReason'], 'consent_declined')
        self.broker.assert_not_called()
        self.assertFalse(any('/Recordings' in call.args[0] for call in self.provider.call_args_list))

    def test_expired_consent_and_session_deadline_never_mint(self):
        self.configure(); self.callback()
        self.now.return_value = 1030
        self.assertIn('<Hangup', self.callback('consent', Digits='1')['body'])
        self.assertNotIn('pinAttempts', self.item())
        self.now.return_value = 1180
        self.assertIn('<Hangup', self.callback('consent', Digits='1')['body'])
        self.broker.assert_not_called()

    def test_invalid_account_destination_and_caller_do_not_advance_consent(self):
        self.configure(); self.callback()
        for fields in ({'AccountSid': 'AC'+'9'*32}, {'From': '+12025550999'}, {'To': '+12025550999'}):
            self.assertEqual(self.callback('consent', Digits='1', **fields)['statusCode'], 403)
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
        self.configure(); self.callback(); self.callback('consent', Digits='1')
        self.control()
        raw = json.dumps({'capability': self.item()['callbackId'], 'callSid': f.CALL,
                          'transcriptFinalized': True, 'reason': 'remote_hangup', 'inputTokens': 123, 'outputTokens': 45}).encode()
        self.assertEqual(i.redeem(raw, {'x-ln-signature': r.digest(b'inbound-owner/finish\n' + raw)}, 'finish')['statusCode'], 200)
        self.callback('status', CallStatus='completed')
        self.final.assert_not_called()
        self.assertTrue(self.item()['transcriptFinalized'])
        self.assertEqual(self.item()['endReason'], 'remote_hangup')
        self.assertEqual(self.item()['inputTokens'], 123)

    def test_fresh_account_revocation_blocks_media_heartbeat(self):
        self.configure(); self.callback(); self.callback('consent', Digits='1')
        self.control()
        self.owner_active.return_value = False
        self.assertEqual(self.control('check')['statusCode'], 403)
        self.owner_active.assert_called_with(owners.OWNER, since=1000)

    def test_control_signature_cannot_be_replayed_across_actions(self):
        self.configure(); self.callback(); self.callback('consent', Digits='1')
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
        self.configure(); self.callback(); self.callback('consent', Digits='1')
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

    def test_recording_timeout_keeps_attempted_intent_for_late_cleanup(self):
        self.configure(); self.callback()
        def timeout(path, form=None):
            if path.endswith('/Recordings.json'):
                self.assertEqual(r.get(r.call_key(f.CALL, m.namespace(owners.OWNER)))['recordingCreateState'], 'attempted')
                raise m.ProviderError()
            return self.provider_result(path, form)
        self.provider.side_effect = timeout
        self.callback('consent', Digits='1')
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
        self.callback('consent', Digits='1')
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
            'surface': 'phone', 'phoneCallSid': f.CALL, 'phoneDeadline': 1180})


if __name__ == '__main__':
    unittest.main()
