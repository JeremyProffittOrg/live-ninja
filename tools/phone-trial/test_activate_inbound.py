import copy
from datetime import datetime, timezone
from decimal import Decimal
import json
import types
import unittest
from unittest.mock import patch

import activate_inbound as a

NOW = datetime(2026,10,17,21,0,tzinfo=timezone.utc)
OWNER = '11111111-1111-4111-8111-111111111111'
ACCOUNT = 'AC'+'1'*32
NUMBER = 'PN'+'2'*32
CALL = 'CA'+'3'*32
SERVICE = 'https://example123.execute-api.us-east-1.amazonaws.com'


class Table:
    def __init__(self, rows):
        self.rows=rows;self.transactions=[];self.reads=[]
        self.meta=types.SimpleNamespace(client=types.SimpleNamespace(transact_write_items=self.commit))

    def get_item(self,Key,ProjectionExpression,ExpressionAttributeNames=None,**kwargs):
        self.reads.append(ProjectionExpression)
        key=tuple(Key.values());row=self.rows.get(key,{})
        fields=[(ExpressionAttributeNames or {}).get(field,field) for field in ProjectionExpression.split(',')]
        return {'Item':{field:copy.deepcopy(row[field]) for field in fields if field in row}}

    def commit(self,TransactItems):
        self.transactions.append(TransactItems)
        # Check all voice/budget guards before applying either enable flag.
        for operation in TransactItems:
            kind,value=next(iter(operation.items()))
            key=value['Key'];values=value.get('ExpressionAttributeValues',{})
            if key==a.VOICE:
                active=self.rows.get(tuple(key.values()),{}).get('activeRequest')
                if kind=='Update':a.check(active==values[':stop'],'voice_changed')
                else:a.check(active is None,'voice_changed')
            if key.get('SK','').startswith('DAY#'):
                a.check(self.rows[tuple(key.values())]['version']==values[':v'],'budget_changed')
        for operation in TransactItems:
            if 'Update' not in operation:continue
            value=operation['Update'];key=value['Key'];row=self.rows[tuple(key.values())]
            if key==a.VOICE:row.pop('activeRequest')
            else:
                row['enabled']=True
                if key==a.INBOUND:row['version']=value['ExpressionAttributeValues'][':next']


class Session:
    def __init__(self):
        self.main=Table({('CONFIG','OWNER'):{'userId':OWNER},('USER#'+OWNER,'PROFILE'):{'role':'owner','status':'active'}})
        self.phone=Table({
            ('CONFIG','OWNER_MESSAGES'):{'ownerUserId':OWNER,'enabled':True,'fromNumber':'+12025550100','destinations':{'primary':'+12025550101','secondary':'+12025550102'},'timezone':'America/New_York'},
            ('CONFIG','INBOUND_OWNER'):{'ownerUserId':OWNER,'enabled':False,'version':1},
            ('PHONE#BUDGET#2026-10','DAY#2026-10-17'):{'version':1,'grantedUSD':Decimal('10'),'spentUSD':Decimal('0'),'expiresAt':1792296000,'timezone':'America/New_York'},
            ('OWNER#'+OWNER,'BUDGET_GRANT#'+a.RECEIPT):{'day':a.DATE,'amountUSD':Decimal('10'),'authorizedTotalUSD':Decimal('10'),'authorizationScope':'owner-only-parked-car-tests','expiresAt':1792296000},
            ('PHONE#CONTROL','VOICE'):{}
        })
    def client(self,name,**_):
        if name=='sts':return types.SimpleNamespace(get_caller_identity=lambda:{'Account':a.ACCOUNT})
        if name=='cloudformation':return types.SimpleNamespace(describe_stacks=lambda **_:{'Stacks':[{'StackStatus':'UPDATE_COMPLETE','Outputs':[{'OutputKey':'ServiceUrl','OutputValue':SERVICE}]}]})
        raise AssertionError(name)
    def resource(self,name,**_):
        assert name=='dynamodb'
        return types.SimpleNamespace(Table=lambda name:self.main if name=='live-ninja' else self.phone)


class Provider:
    account=ACCOUNT
    def __init__(self):
        self.posts=[];self.active=[];self.after_write=None
        self.number={'sid':NUMBER,'account_sid':ACCOUNT,'phone_number':'+12025550100','capabilities':{'voice':True},'voice_url':'https://azure.example/unavailable','voice_method':'GET','status_callback':'','status_callback_method':'POST','sms_url':'https://sms.example/keep','sms_method':'POST','voice_application_sid':'','trunk_sid':None}
    def request(self,path,form=None,monitor=False):
        if monitor:return {'alerts':[{'account_sid':ACCOUNT,'resource_sid':CALL,'error_code':'11200','request_url':'https://azure.example/unavailable?pin=123456&token=TOPSECRET','alert_text':'HttpResponse=404&private=TOPSECRET'}]}
        if path.startswith('/IncomingPhoneNumbers.json?'):return {'incoming_phone_numbers':[copy.deepcopy(self.number)]}
        if path.startswith('/IncomingPhoneNumbers/'):
            if form:
                self.posts.append(form)
                self.number.update(voice_url=form['VoiceUrl'],voice_method=form['VoiceMethod'],status_callback=form['StatusCallback'],status_callback_method=form['StatusCallbackMethod'])
                if self.after_write:self.after_write()
            return copy.deepcopy(self.number)
        if 'Status=' in path:return {'calls':self.active}
        if path.startswith('/Calls.json?'):return {'calls':[{'sid':CALL,'account_sid':ACCOUNT,'direction':'inbound','to':'+12025550100','from':'+12025550101'}]}
        raise AssertionError('unexpected provider path')


class InboundOperationTests(unittest.TestCase):
    def fixture(self):
        session=Session();provider=Provider();state=a.load_state(session,NOW)
        number,expected,summary=a.observe(provider,state,NOW)
        return session,provider,state,number,expected,summary
    def activate(self,state,provider,number,expected,summary):
        with patch.object(a,'datetime') as clock:
            clock.now.return_value=NOW
            return a.activate(provider,state,number,expected,summary,NOW)

    def test_readonly_red_probe_exposes_actual_wrong_route_without_private_values_or_writes(self):
        session,provider,state,number,expected,summary=self.fixture()
        self.assertFalse(summary['routeMatches']);self.assertEqual(summary['voicePath'],'/unavailable');self.assertEqual(summary['voiceMethod'],'GET')
        self.assertFalse(summary['inboundReady'])
        self.assertTrue(summary['ownerAdmissionConfigured']);self.assertNotIn('pinEnrolled',summary)
        self.assertTrue(summary['latestInboundCallerAllowlisted']);self.assertEqual(summary['debuggerErrors'][0]['httpStatus'],404)
        text=json.dumps(summary)
        for value in ('+1202','123456','TOPSECRET','pinHash','pinSalt'):self.assertNotIn(value,text)
        self.assertFalse(provider.posts);self.assertFalse(session.phone.transactions)
        self.assertNotIn('pinHash',state['inbound']);self.assertNotIn('pinSalt',state['inbound'])

    def test_activation_only_updates_four_route_fields_then_enables_flags(self):
        for stop in (None,a.STOP):
            with self.subTest(stop=stop):
                session,provider,state,number,expected,summary=self.fixture()
                if stop:session.phone.rows[('PHONE#CONTROL','VOICE')]['activeRequest']=stop;state['lock']={'activeRequest':stop}
                result=self.activate(state,provider,number,expected,summary)
                self.assertEqual(result['callsMade'],0)
                self.assertEqual(set(provider.posts[0]),{'VoiceUrl','VoiceMethod','StatusCallback','StatusCallbackMethod'})
                self.assertEqual(provider.number['sms_url'],'https://sms.example/keep')
                self.assertEqual(provider.number['status_callback'],SERVICE+'/inbound-owner/status')
                self.assertTrue(session.phone.rows[('CONFIG','INBOUND_OWNER')]['enabled'])
                self.assertNotIn('activeRequest',session.phone.rows[('PHONE#CONTROL','VOICE')])
                self.assertNotIn('pin',json.dumps(session.phone.transactions,default=str).lower())

    def test_active_shared_or_provider_call_blocks_before_route_write(self):
        for kind in ('lock','provider'):
            session,provider,state,number,expected,summary=self.fixture()
            if kind=='lock':state['lock']={'activeRequest':'owner:active-call'}
            else:summary['ownedActiveCalls']=1
            self.assertRaises(a.Blocked,self.activate,state,provider,number,expected,summary)
            self.assertFalse(provider.posts);self.assertFalse(session.phone.transactions)

    def test_concurrent_voice_lock_never_gets_cleared(self):
        session,provider,state,number,expected,summary=self.fixture()
        provider.after_write=lambda:session.phone.rows[('PHONE#CONTROL','VOICE')].update(activeRequest='owner:real-call')
        self.assertRaises(a.Blocked,self.activate,state,provider,number,expected,summary)
        self.assertEqual(session.phone.rows[('PHONE#CONTROL','VOICE')]['activeRequest'],'owner:real-call')
        self.assertFalse(session.phone.rows[('CONFIG','INBOUND_OWNER')]['enabled'])

    def test_sms_readback_change_prevents_enable(self):
        session,provider,state,number,expected,summary=self.fixture()
        provider.after_write=lambda:provider.number.update(sms_url='https://changed.example')
        self.assertRaises(a.Blocked,self.activate,state,provider,number,expected,summary)
        self.assertFalse(session.phone.transactions)

    def test_bad_budget_override_or_wrong_date_cannot_activate(self):
        for field in ('budgetReady','routingOverride'):
            session,provider,state,number,expected,summary=self.fixture()
            if field=='budgetReady':state[field]=False
            else:summary[field]=True
            self.assertRaises(a.Blocked,self.activate,state,provider,number,expected,summary)
            self.assertFalse(provider.posts)
        state=a.load_state(Session(),datetime(2026,10,18,12,tzinfo=timezone.utc));self.assertFalse(state['budgetReady'])

    def test_invalid_owner_admission_fails_without_secrets_projection(self):
        for field,value in [('ownerUserId','another-owner'),('enabled',None),('version',0)]:
            with self.subTest(field=field):
                session=Session();session.phone.rows[('CONFIG','INBOUND_OWNER')][field]=value
                self.assertRaises(a.Blocked,a.load_state,session,NOW)
                self.assertTrue(all('pin' not in value.lower() for value in session.phone.reads))

    def test_correct_route_with_disabled_inbound_never_claims_ready(self):
        session,provider,state,number,expected,summary=self.fixture()
        provider.number.update(expected)
        _,_,summary=a.observe(provider,state,NOW)
        self.assertTrue(summary['routeMatches']);self.assertFalse(summary['inboundReady'])

    def test_recording_deletion_marker_blocks_activation(self):
        session=Session();session.phone.rows[('RUN#owner-messages#'+OWNER,'ACCOUNT_DELETION')]={'PK':'RUN#owner-messages#'+OWNER}
        self.assertRaises(a.Blocked,a.load_state,session,NOW)
        self.assertTrue(all('pinHash' not in value and 'pinSalt' not in value for value in session.phone.reads))
        session=Session();session.main.rows[('USER#'+OWNER,'PROFILE')]['status']='disabled'
        self.assertRaises(a.Blocked,a.load_state,session,NOW)

    def test_github_activation_requires_exact_repository_main_manual_and_date(self):
        env={'GITHUB_ACTIONS':'true','GITHUB_REPOSITORY':a.REPOSITORY,'GITHUB_REF':'refs/heads/main','GITHUB_EVENT_NAME':'workflow_dispatch','ACTIVATION_DATE':a.DATE}
        with patch.dict(a.os.environ,env,clear=True):
            a.context_guard(True)
            for key,value in [('GITHUB_REPOSITORY','other/repo'),('GITHUB_REF','refs/heads/test'),('GITHUB_EVENT_NAME','push'),('ACTIVATION_DATE','2026-10-18')]:
                with patch.dict(a.os.environ,{key:value}):self.assertRaises(a.Blocked,a.context_guard,True)


if __name__=='__main__':unittest.main()
