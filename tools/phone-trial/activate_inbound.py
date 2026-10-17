"""Dated GitHub-only inbound routing diagnostic/activation. Never originates calls."""
import argparse
import base64
from datetime import datetime, timedelta, timezone
from decimal import Decimal
import json
import os
from pathlib import Path
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from zoneinfo import ZoneInfo

ACCOUNT = '759775734231'
REPOSITORY = 'JeremyProffittOrg/live-ninja'
DATE = '2026-10-17'
RECEIPT = 'parked-car-2026-10-17-total-10'
STOP = 'owner-stop-2026-10-16'
CONFIG = {'PK': 'CONFIG', 'SK': 'OWNER_MESSAGES'}
INBOUND = {'PK': 'CONFIG', 'SK': 'INBOUND_OWNER'}
VOICE = {'PK': 'PHONE#CONTROL', 'SK': 'VOICE'}


class Blocked(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def check(condition, reason):
    if not condition:
        raise Blocked(reason)


def context_guard(activate):
    check(os.environ.get('GITHUB_ACTIONS') == 'true' and os.environ.get('GITHUB_REPOSITORY') == REPOSITORY
          and os.environ.get('GITHUB_REF') == 'refs/heads/main', 'wrong_github_context')
    if activate:
        check(os.environ.get('GITHUB_EVENT_NAME') == 'workflow_dispatch'
              and os.environ.get('ACTIVATION_DATE') == DATE, 'explicit_dated_activation_required')


class Provider:
    def __init__(self):
        self.account = os.environ.get('TWILIO_ACCOUNT_SID', '')
        key, secret = os.environ.get('TWILIO_API_KEY_SID', ''), os.environ.get('TWILIO_API_KEY_SECRET', '')
        check(re.fullmatch('AC[0-9a-fA-F]{32}', self.account) and re.fullmatch('SK[0-9a-fA-F]{32}', key) and secret, 'provider_credentials_unavailable')
        self.authorization = 'Basic ' + base64.b64encode((key + ':' + secret).encode()).decode()

    def request(self, path, form=None, monitor=False):
        if monitor:
            check(form is None and path.startswith('/v1/Alerts?'), 'invalid_monitor_operation')
            url = 'https://monitor.twilio.com' + path
        else:
            check(path.startswith(('/IncomingPhoneNumbers', '/Calls.json')), 'invalid_provider_operation')
            if form is not None:
                check(re.fullmatch('/IncomingPhoneNumbers/PN[0-9a-fA-F]{32}.json', path)
                      and set(form) == {'VoiceUrl', 'VoiceMethod', 'StatusCallback', 'StatusCallbackMethod'}, 'invalid_routing_write')
            url = 'https://api.twilio.com/2010-04-01/Accounts/' + self.account + path
        body = urllib.parse.urlencode(form).encode() if form is not None else None
        request = urllib.request.Request(url, data=body, headers={'Authorization': self.authorization, 'Content-Type': 'application/x-www-form-urlencoded'})
        try:
            with urllib.request.build_opener(NoRedirect).open(request, timeout=15) as response:
                raw = response.read(1024 * 1024 + 1)
                check(len(raw) <= 1024 * 1024, 'provider_response_too_large')
                return json.loads(raw)
        except urllib.error.HTTPError as exc:
            raise Blocked('provider_http_' + str(exc.code)) from None
        except Blocked:
            raise
        except Exception:
            raise Blocked('provider_request_failed_no_response_logged') from None


def get(table, key, projection, names=None):
    args = dict(Key=key, ProjectionExpression=projection, ConsistentRead=True)
    if names:
        args['ExpressionAttributeNames'] = names
    return table.get_item(**args).get('Item', {})


def load_state(session, now):
    check(session.client('sts').get_caller_identity()['Account'] == ACCOUNT, 'wrong_aws_account')
    resource = session.resource('dynamodb', region_name='us-east-1')
    main, phone = resource.Table('live-ninja'), resource.Table('live-ninja-phone-trial')
    owner = get(main, {'pk': 'CONFIG', 'sk': 'OWNER'}, 'userId').get('userId')
    check(isinstance(owner, str) and re.fullmatch('[A-Za-z0-9-]{1,100}', owner), 'owner_binding_unavailable')
    profile = get(main, {'pk': 'USER#' + owner, 'sk': 'PROFILE'}, '#role,#status', {'#role':'role','#status':'status'})
    check(profile.get('role') == 'owner' and profile.get('status') == 'active', 'owner_not_active')
    cfg = get(phone, CONFIG, 'ownerUserId,enabled,fromNumber,destinations,#zone', {'#zone':'timezone'})
    check(cfg.get('ownerUserId') == owner and isinstance(cfg.get('enabled'), bool), 'phone_owner_mismatch')
    targets = cfg.get('destinations', {})
    check(set(targets) == {'primary', 'secondary'} and len(set(targets.values())) == 2
          and all(isinstance(n, str) and re.fullmatch(r'\+[1-9][0-9]{7,14}', n) for n in targets.values())
          and re.fullmatch(r'\+[1-9][0-9]{7,14}', cfg.get('fromNumber', '')), 'exact_two_callers_required')
    inbound = get(phone, INBOUND, 'ownerUserId,enabled,#v,pinIterations', {'#v':'version'})
    pin = phone.query(KeyConditionExpression='PK=:pk AND SK=:sk',
        FilterExpression='ownerUserId=:owner AND pinIterations=:iterations AND size(pinHash)=:hashlen AND size(pinSalt)=:saltlen',
        ExpressionAttributeValues={':pk':'CONFIG',':sk':'INBOUND_OWNER',':owner':owner,':iterations':600000,':hashlen':64,':saltlen':32},
        Select='COUNT', ConsistentRead=True).get('Count') == 1
    check(pin and inbound.get('ownerUserId') == owner and isinstance(inbound.get('enabled'), bool)
          and int(inbound.get('version', 0)) >= 1, 'pin_enrollment_unavailable')
    check(not get(phone, {'PK':'OWNER#'+owner,'SK':'ACCOUNT_DELETION'}, 'PK'), 'account_deletion_pending')
    check(not get(phone, {'PK':'RUN#owner-messages#'+owner,'SK':'ACCOUNT_DELETION'}, 'PK'), 'recording_deletion_pending')
    zone = cfg.get('timezone') or 'America/New_York'
    local = now.astimezone(ZoneInfo(zone)); day = local.date().isoformat(); month = local.strftime('%Y-%m')
    budget_key = {'PK':'PHONE#BUDGET#'+month,'SK':'DAY#'+day}
    budget = get(phone, budget_key, 'grantedUSD,spentUSD,expiresAt,#v,#zone', {'#v':'version','#zone':'timezone'})
    receipt_key = {'PK':'OWNER#'+owner,'SK':'BUDGET_GRANT#'+RECEIPT}
    receipt = get(phone, receipt_key, '#day,amountUSD,authorizedTotalUSD,authorizationScope,expiresAt', {'#day':'day'})
    remaining = Decimal(str(budget.get('grantedUSD', '0'))) - Decimal(str(budget.get('spentUSD', '0')))
    ready = (day == DATE and Decimal(str(budget.get('grantedUSD', '0'))) == Decimal('10') and remaining >= Decimal('1.40')
             and int(budget.get('expiresAt', 0)) > int(now.timestamp()) and int(budget.get('version', 0)) >= 1
             and budget.get('timezone') == zone and receipt.get('day') == DATE
             and receipt.get('authorizationScope') == 'owner-only-parked-car-tests' and receipt.get('expiresAt') == budget.get('expiresAt')
             and Decimal(str(receipt.get('authorizedTotalUSD', '0'))) == Decimal('10')
             and Decimal(str(receipt.get('amountUSD', '0'))) == Decimal('10'))
    stack = session.client('cloudformation', region_name='us-east-1').describe_stacks(StackName='live-ninja-phone-trial')['Stacks'][0]
    check(stack['StackStatus'] in {'CREATE_COMPLETE','UPDATE_COMPLETE'}, 'recorder_deployment_incomplete')
    service = next((item['OutputValue'] for item in stack['Outputs'] if item['OutputKey'] == 'ServiceUrl'), '')
    check(re.fullmatch(r'https://[a-z0-9]+\.execute-api\.us-east-1\.amazonaws\.com', service), 'invalid_recorder_origin')
    return {'owner':owner,'cfg':cfg,'inbound':inbound,'budget':budget,'budgetKey':budget_key,'receiptKey':receipt_key,
            'remaining':remaining,'budgetReady':ready,'service':service,'phone':phone,'lock':get(phone,VOICE,'activeRequest')}


def safe_path(raw):
    path = urllib.parse.urlsplit(raw or '').path
    known = {'/voice','/unavailable','/consent','/navigate','/health','/inbound-owner/voice','/inbound-owner/status'}
    if path in known:
        return path
    for prefix in ('/inbound-owner/', '/owner-message/', '/media/'):
        if path.startswith(prefix):
            return prefix + '<redacted>'
    return '/<other-route>' if path else ''


def safe_http_status(alert):
    text = str(alert.get('alert_text', ''))
    try:
        fields = json.loads(text)
    except (ValueError, TypeError):
        fields = {key:value[0] for key,value in urllib.parse.parse_qs(text).items() if value}
    if isinstance(fields, dict):
        for key in ('HttpResponse','httpResponse','ResponseStatusCode','response_status_code','statusCode'):
            value = str(fields.get(key, ''))
            if re.fullmatch('[1-5][0-9]{2}', value):
                return int(value)
    match = re.search(r'\bHTTP(?:/1\.[01])?\s+([1-5][0-9]{2})\b', text)
    return int(match[1]) if match else None


def observe(provider, state, now):
    cfg = state['cfg']
    numbers = provider.request('/IncomingPhoneNumbers.json?' + urllib.parse.urlencode({'PhoneNumber':cfg['fromNumber'],'PageSize':1000}))
    check(not numbers.get('next_page_uri'), 'number_listing_requires_reconciliation')
    matches = [n for n in numbers.get('incoming_phone_numbers', []) if n.get('phone_number') == cfg['fromNumber']]
    check(len(matches) == 1, 'exact_owned_number_not_found')
    number = matches[0]
    check(number.get('account_sid') == provider.account and re.fullmatch('PN[0-9a-fA-F]{32}', number.get('sid',''))
          and number.get('capabilities',{}).get('voice') is True, 'number_identity_or_voice_mismatch')
    expected = {'voice_url':state['service']+'/inbound-owner/voice','voice_method':'POST',
                'status_callback':state['service']+'/inbound-owner/status','status_callback_method':'POST'}
    route_ok = all(number.get(key) == value for key,value in expected.items())
    override = bool(number.get('voice_application_sid') or number.get('trunk_sid')) or number.get('voice_receive_mode','voice') != 'voice'
    recent = provider.request('/Calls.json?' + urllib.parse.urlencode({'To':cfg['fromNumber'],'PageSize':10}))
    calls = [c for c in recent.get('calls',[]) if c.get('to') == cfg['fromNumber'] and c.get('account_sid') == provider.account and c.get('direction') == 'inbound' and re.fullmatch('CA[0-9a-fA-F]{32}',c.get('sid',''))]
    call_ids = {c['sid'] for c in calls}
    latest = calls[0] if calls else None
    diagnostics = []
    alerts_ok = True
    try:
        alerts = provider.request('/v1/Alerts?' + urllib.parse.urlencode({'LogLevel':'error','StartDate':(now-timedelta(hours=2)).strftime('%Y-%m-%dT%H:%M:%SZ'),'PageSize':50}), monitor=True)
        for alert in alerts.get('alerts',[]):
            code = str(alert.get('error_code',''))
            if alert.get('account_sid') == provider.account and alert.get('resource_sid') in call_ids and re.fullmatch('[0-9]{3,6}',code):
                diagnostics.append({'callSid':alert['resource_sid'],'errorCode':code,'requestPath':safe_path(alert.get('request_url','')),'httpStatus':safe_http_status(alert)})
    except Blocked:
        alerts_ok = False
    active = 0
    for status in ('queued','ringing','in-progress'):
        page = provider.request('/Calls.json?' + urllib.parse.urlencode({'Status':status,'PageSize':1000}))
        check(not page.get('next_page_uri'), 'active_call_listing_requires_reconciliation')
        active += sum(1 for call in page.get('calls',[]) if cfg['fromNumber'] in (call.get('from'),call.get('to')))
    summary = {'routeMatches':route_ok,'voicePath':safe_path(number.get('voice_url','')),'voiceMethod':number.get('voice_method') if number.get('voice_method') in {'GET','POST'} else 'unknown',
               'statusCallbackPath':safe_path(number.get('status_callback','')),'statusCallbackMethod':number.get('status_callback_method') if number.get('status_callback_method') in {'GET','POST'} else 'unknown',
               'routingOverride':override,'ownedActiveCalls':active,'ownerEnabled':cfg['enabled'],'inboundEnabled':state['inbound']['enabled'],
               'pinEnrolled':True,'budgetReady':state['budgetReady'],'remainingUSD':str(state['remaining']),
               'latestInboundCallSid':latest['sid'] if latest else None,'latestInboundCallerAllowlisted':latest.get('from') in cfg['destinations'].values() if latest else None,
               'debuggerReadSucceeded':alerts_ok,'debuggerErrors':diagnostics[:5]}
    summary['inboundReady'] = (route_ok and not override and active == 0 and not state['lock'].get('activeRequest')
                               and cfg['enabled'] and state['inbound']['enabled'] and state['budgetReady'])
    return number, expected, summary


def activate(provider, state, number, expected, summary, now):
    check(state['budgetReady'], 'current_dated_ten_dollar_budget_required')
    check(not summary['routingOverride'], 'existing_application_or_trunk_routing_must_be_resolved')
    check(summary['ownedActiveCalls'] == 0, 'owned_call_is_active')
    active = state['lock'].get('activeRequest')
    check(active in (None, STOP), 'shared_voice_lock_is_active')
    preserved = {key:number.get(key) for key in ('sms_url','sms_method','sms_application_sid','sms_fallback_url','sms_fallback_method','voice_application_sid','trunk_sid','voice_fallback_url','voice_fallback_method','voice_caller_id_lookup')}
    if not summary['routeMatches']:
        provider.request('/IncomingPhoneNumbers/'+number['sid']+'.json',
                         {'VoiceUrl':expected['voice_url'],'VoiceMethod':'POST','StatusCallback':expected['status_callback'],'StatusCallbackMethod':'POST'})
    verified = provider.request('/IncomingPhoneNumbers/'+number['sid']+'.json')
    check(verified.get('sid') == number['sid'] and verified.get('account_sid') == provider.account and verified.get('phone_number') == state['cfg']['fromNumber']
          and all(verified.get(k) == v for k,v in expected.items()) and all(verified.get(k) == v for k,v in preserved.items()), 'routing_readback_mismatch')
    check(datetime.now(timezone.utc).timestamp() < int(state['budget']['expiresAt']), 'budget_expired_before_enable')
    owner, cfg, inbound, budget = state['owner'], state['cfg'], state['inbound'], state['budget']
    operations = [
        {'ConditionCheck':{'TableName':'live-ninja','Key':{'pk':'CONFIG','sk':'OWNER'},'ConditionExpression':'userId=:owner','ExpressionAttributeValues':{':owner':owner}}},
        {'ConditionCheck':{'TableName':'live-ninja','Key':{'pk':'USER#'+owner,'sk':'PROFILE'},'ConditionExpression':'#role=:owner AND #status=:active','ExpressionAttributeNames':{'#role':'role','#status':'status'},'ExpressionAttributeValues':{':owner':'owner',':active':'active'}}},
        {'ConditionCheck':{'TableName':'live-ninja-phone-trial','Key':{'PK':'OWNER#'+owner,'SK':'ACCOUNT_DELETION'},'ConditionExpression':'attribute_not_exists(PK)'}},
        {'ConditionCheck':{'TableName':'live-ninja-phone-trial','Key':{'PK':'RUN#owner-messages#'+owner,'SK':'ACCOUNT_DELETION'},'ConditionExpression':'attribute_not_exists(PK)'}},
        {'ConditionCheck':{'TableName':'live-ninja-phone-trial','Key':state['budgetKey'],'ConditionExpression':'#v=:v AND grantedUSD=:ten AND spentUSD=:spent AND expiresAt=:expiry','ExpressionAttributeNames':{'#v':'version'},'ExpressionAttributeValues':{':v':budget['version'],':ten':Decimal('10'),':spent':budget.get('spentUSD',Decimal(0)),':expiry':budget['expiresAt']}}},
        {'ConditionCheck':{'TableName':'live-ninja-phone-trial','Key':state['receiptKey'],'ConditionExpression':'#day=:day AND amountUSD=:ten AND authorizedTotalUSD=:ten AND expiresAt=:expiry AND authorizationScope=:scope','ExpressionAttributeNames':{'#day':'day'},'ExpressionAttributeValues':{':day':DATE,':ten':Decimal('10'),':expiry':budget['expiresAt'],':scope':'owner-only-parked-car-tests'}}},
        {'Update':{'TableName':'live-ninja-phone-trial','Key':CONFIG,'UpdateExpression':'SET #enabled=:yes','ConditionExpression':'ownerUserId=:owner AND fromNumber=:number AND destinations=:targets AND #enabled=:old','ExpressionAttributeNames':{'#enabled':'enabled'},'ExpressionAttributeValues':{':owner':owner,':number':cfg['fromNumber'],':targets':cfg['destinations'],':old':cfg['enabled'],':yes':True}}},
        {'Update':{'TableName':'live-ninja-phone-trial','Key':INBOUND,'UpdateExpression':'SET #enabled=:yes, #v=:next','ConditionExpression':'ownerUserId=:owner AND #v=:v AND #enabled=:old AND pinIterations=:iterations AND size(pinHash)=:hashlen AND size(pinSalt)=:saltlen','ExpressionAttributeNames':{'#enabled':'enabled','#v':'version'},'ExpressionAttributeValues':{':owner':owner,':v':inbound['version'],':next':int(inbound['version'])+1,':old':inbound['enabled'],':yes':True,':iterations':600000,':hashlen':64,':saltlen':32}}}
    ]
    if active == STOP:
        operations.append({'Update':{'TableName':'live-ninja-phone-trial','Key':VOICE,'UpdateExpression':'REMOVE activeRequest','ConditionExpression':'activeRequest=:stop','ExpressionAttributeValues':{':stop':STOP}}})
    else:
        operations.append({'ConditionCheck':{'TableName':'live-ninja-phone-trial','Key':VOICE,'ConditionExpression':'attribute_not_exists(activeRequest)'}})
    state['phone'].meta.client.transact_write_items(TransactItems=operations)
    enabled = get(state['phone'], INBOUND, 'enabled')
    owner_enabled = get(state['phone'], CONFIG, 'enabled')
    check(enabled.get('enabled') is True and owner_enabled.get('enabled') is True, 'enable_readback_mismatch')
    return {'activated':True,'routeMatches':True,'inboundEnabled':True,'ownerEnabled':True,'callsMade':0}


def main():
    parser = argparse.ArgumentParser();parser.add_argument('--activate',action='store_true');args=parser.parse_args()
    try:
        context_guard(args.activate)
        import boto3
        now=datetime.now(timezone.utc);state=load_state(boto3.Session(region_name='us-east-1'),now)
        provider=Provider();number,expected,summary=observe(provider,state,now)
        print(json.dumps(summary,sort_keys=True))
        if not args.activate:
            check(summary['routeMatches'] and not summary['routingOverride'], 'inbound_route_red')
            check(summary['inboundReady'], 'inbound_readiness_red')
            print('PASS: read-only inbound route and admission ready; no calls or writes.');return
        bridge=json.loads((Path(__file__).resolve().parents[2]/'infra/phone-trial-config.json').read_text())['inboundBridgeUrl']
        with urllib.request.build_opener(NoRedirect).open(bridge+'/health',timeout=20) as response:
            check(response.status==200 and json.load(response).get('ready') is True,'azure_bridge_not_ready')
        print(json.dumps(activate(provider,state,number,expected,summary,now),sort_keys=True))
    except Blocked as exc:
        print('BLOCKED: '+str(exc));raise SystemExit(1) from None
    except Exception:
        print('BLOCKED: operational check failed; no private response or credentials logged.');raise SystemExit(1) from None


if __name__=='__main__':
    main()
