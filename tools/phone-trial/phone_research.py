"""Owner-approved five-hotel Jobs. One bounded attempt per catalog property."""
import hashlib
import json
import os
from pathlib import Path
import re
import time
from datetime import datetime
from decimal import Decimal
from zoneinfo import ZoneInfo

import account_lifecycle as lifecycle
import inbound_owner as inbound
import owner_messages as m
import recorder as r

RESERVE = Decimal('1.40')
MAX_SECONDS = 180
WORKER = 'live-ninja-jobs-worker'
ID = re.compile(r'^(?:job|run)_[0-9a-f]{32}$')
PURPOSE = 'Ask each hotel about guest Wi-Fi login requirements and reported VPN restrictions. Do not request private router settings or credentials.'


def catalog():
    source = json.loads(Path(__file__).with_name('targets.json').read_text(encoding='utf-8'))
    if source.get('runId') != os.environ['RUN_ID']:
        raise ValueError('Bridge catalog namespace mismatch')
    return [{'id': h['id'], 'name': h['name'], 'city': h['city'], 'phone': h['phone'], 'sourceUrl': h['source']}
            for h in source['hotels']]


def control_key(owner, job, run):
    return {'PK': 'OWNER#' + owner, 'SK': 'PHONE_RESEARCH#' + job + '#' + run}


def request_id(job, run, target):
    return 'research-' + hashlib.sha256((job + '\n' + run + '\n' + target).encode()).hexdigest()


def attempt(owner, job, run, target):
    return r.get(m.request_key(owner, request_id(job, run, target)))


def main_job(owner, job):
    import boto3
    return boto3.resource('dynamodb', region_name='us-east-1').Table('live-ninja').get_item(
        Key={'pk': 'USER#' + owner, 'sk': 'JOB#' + job}, ConsistentRead=True).get('Item', {})


def authorize(owner, job, run, event=None, dispatch=False):
    if not isinstance(job, str) or not job.startswith('job_') or not ID.fullmatch(job) or not isinstance(run, str) or not run.startswith('run_') or not ID.fullmatch(run):
        raise ValueError('Invalid job identity')
    cfg = m.config(owner)
    if not cfg or not lifecycle.owner_active(owner) or os.environ['RUN_ID'] not in cfg.get('recordingRuns', []):
        raise ValueError('Owner phone research is unavailable')
    row = main_job(owner, job)
    stored = json.loads(row.get('payload', '{}'))
    definition = stored.get('job', {})
    current = next((value for value in stored.get('runs', []) if value.get('id') == run), None)
    if stored.get('userId') != owner or definition.get('id') != job or not current or current.get('jobId') != job or current.get('kind') != 'phone-research':
        raise ValueError('Phone job owner binding mismatch')
    snapshot = current.get('phoneResearch', {})
    plan = definition.get('phoneResearch', {})
    targets = snapshot.get('targets', [])
    approved = {target['id']: target for target in catalog()}
    if len(targets) != 5 or len({value.get('id') for value in targets}) != 5 or any(value != approved.get(value.get('id')) for value in targets):
        raise ValueError('Exactly five unchanged catalog targets are required')
    if snapshot.get('purpose') != PURPOSE or snapshot.get('maxCallSeconds') != MAX_SECONDS or snapshot.get('maxAttempts') != 1 or Decimal(str(snapshot.get('maxCostUSD', '0'))) != 5 * RESERVE:
        raise ValueError('Phone job limits changed')
    if dispatch and (definition.get('kind') != 'phone-research' or current.get('generation') != stored.get('generation')
                     or any(plan.get(key) != snapshot.get(key) for key in ('targets', 'purpose', 'maxCallSeconds', 'maxAttempts', 'maxCostUSD'))):
        raise ValueError('Phone job generation or definition changed')
    if current.get('approvedBy') != owner or not current.get('approvalExpiresAt'):
        raise ValueError('Phone job approval binding mismatch')
    expiry = int(datetime.fromisoformat(current['approvalExpiresAt'].replace('Z', '+00:00')).timestamp())
    command, command_version = snapshot.get('command'), snapshot.get('commandVersion')
    if command not in {'run', 'pause', 'cancel'} or type(command_version) is not int or command_version < 1:
        raise ValueError('Phone job command is missing')
    if dispatch and command == 'run' and (definition.get('status') != 'active' or current.get('status') in {'succeeded', 'failed', 'cancelled', 'canceled', 'expired'}):
        raise ValueError('Phone job is no longer runnable')
    if event is not None and any(event.get(key) != value for key, value in {
        'generation': current['generation'], 'command': command, 'commandVersion': command_version,
        'approvalExpiresAt': expiry, 'targetIds': [value['id'] for value in targets]}.items()):
        raise ValueError('Stale phone job command or approval')
    return cfg, row, current, expiry


def save(old, **fields):
    new = {**old, **fields, 'version': int(old.get('version', 0)) + 1, 'updatedAt': int(time.time())}
    m.transact([m.put(new, old if old.get('version') else None),
                *lifecycle.ready_conditions({'ownerUserId': old['ownerUserId'], 'runId': os.environ['RUN_ID']})])
    return new


def evidence(call):
    if call.get('consentStatus') != 'granted':
        return 'No consented interview evidence. Wi-Fi requirements and VPN compatibility remain unknown.'
    page = r.clients()[0].query(KeyConditionExpression='PK = :pk AND begins_with(SK, :prefix)',
        ExpressionAttributeValues={':pk': call['PK'], ':prefix': 'EVENT#' + call['callSid'] + '#'}, ConsistentRead=True, Limit=200)
    excerpts, summaries = [], []
    for row in sorted(page.get('Items', []), key=lambda row: (row.get('createdAt', 0), row.get('SK', ''))):
        event = row.get('data', {})
        data = event.get('data', {})
        if event.get('callSid') != call['callSid']:
            continue
        if event.get('type') == 'transcript' and data.get('speaker') == 'hotel' and data.get('phase') != 'navigation' and isinstance(data.get('text'), str):
            excerpts.append(data['text'][:250])
        if event.get('type') == 'summary' and isinstance(data.get('summary'), str) and data.get('phase') not in {'navigation', 'media'}:
            summaries.append(data['summary'][:1000])
    parts = ['Staff reports only; VPN operation has not been tested.']
    if summaries:
        parts.append('AI summary of staff statements: ' + summaries[-1])
    if excerpts:
        parts.append('Recognized staff speech (may contain transcription errors): ' + ' / '.join(excerpts[-3:]))
    if not summaries and not excerpts:
        parts.append('No interview findings were captured. Connection requirements remain unknown.')
    return '\n'.join(parts)[:2000]


def view(control):
    properties = []
    for target in control['targetIds']:
        item = attempt(control['ownerUserId'], control['jobId'], control['researchRunId'], target)
        value = {'targetId': target, 'status': 'skipped' if control['status'] == 'cancelled' else 'not_started'}
        if item:
            value['status'] = item.get('providerStatus', 'unknown')
            if item.get('callSid'):
                value['callSid'] = item['callSid']
                call = r.get(r.call_key(item['callSid']))
                if call and call.get('ownerUserId') == control['ownerUserId'] and call.get('phoneResearchJobId') == control['jobId'] and call.get('phoneResearchRunId') == control['researchRunId']:
                    value.update(recordingStatus=call.get('recordingStatus', 'pending'), recordingSource=os.environ['RUN_ID'], result=evidence(call))
            if item.get('providerCode'):
                value['error'] = 'Provider rejected the call (code ' + str(item['providerCode']) + ').'
        properties.append(value)
    return {'status': control['status'], 'reason': control.get('reason', ''), 'version': int(control['version']),
            'commandVersion': int(control.get('commandVersion', 0)), 'properties': properties}


def notify(control):
    import boto3
    try:
        result = boto3.client('lambda').invoke(FunctionName=WORKER, InvocationType='Event', Payload=json.dumps({
            'kind': 'phone-research', 'userId': control['ownerUserId'], 'jobId': control['jobId'], 'runId': control['researchRunId']}).encode())
        if result.get('StatusCode') != 202:
            raise RuntimeError('Notification not accepted')
    except Exception:
        try:
            m.transact([{'Update': {'TableName': os.environ['TABLE'], 'Key': {key: control[key] for key in ('PK', 'SK')},
                'UpdateExpression': 'SET notificationFailed=:yes', 'ConditionExpression': 'attribute_exists(PK) AND version=:version AND ownerUserId=:owner',
                'ExpressionAttributeValues': {':yes': True, ':version': control['version'], ':owner': control['ownerUserId']}}},
                *lifecycle.ready_conditions({'ownerUserId': control['ownerUserId'], 'runId': os.environ['RUN_ID']})])
        except Exception as exc:
            if not r.conditional(exc):
                raise
        return False
    return True


def call_status(item, result):
    if result.get('sid') != item.get('callSid') or result.get('account_sid') != os.environ['TWILIO_ACCOUNT_SID'] or result.get('status') not in m.STATUSES:
        raise ValueError('Provider call status binding mismatch')
    return m.change({key: item[key] for key in ('PK', 'SK')}, {'providerStatus': result['status']},
        'research_status', {'callSid': item['callSid'], 'status': result['status']}, sid=item['callSid'], release=result['status'] in m.TERMINAL)


def current_attempt(control):
    for target in control['targetIds']:
        item = attempt(control['ownerUserId'], control['jobId'], control['researchRunId'], target)
        if item and item.get('providerStatus') not in m.TERMINAL | {'rejected'}:
            return item
    return None


def advance(control, event=None, callback=False):
    owner, job, run = control['ownerUserId'], control['jobId'], control['researchRunId']
    if callback and control['status'] != 'running' and not (control['status'] == 'blocked' and control.get('command') in {'pause', 'cancel'}):
        return control
    cfg, main, approved, expiry = authorize(owner, job, run, event, dispatch=True)
    snapshot = approved['phoneResearch']
    command = snapshot['command']
    if snapshot['commandVersion'] < control.get('commandVersion', 0):
        raise ValueError('Stale research command')
    if command != 'run':
        control = save(control, command=command, commandVersion=snapshot['commandVersion'], status='blocked', reason='Provider termination pending for owner ' + command + ' request.')
        active = current_attempt(control)
        if active:
            if not active.get('callSid'):
                return save(control, status='blocked', reason='Call creation outcome is unknown; no further calls are allowed.')
            try:
                stopped = call_status(active, m.provider('/Calls/' + active['callSid'] + '.json', {'Status': 'completed'}))
                if stopped.get('providerStatus') not in m.TERMINAL:
                    raise ValueError('Provider termination remains active')
            except (ValueError, m.ProviderError):
                return save(control, status='blocked', reason='Provider termination is unconfirmed; the voice lock is retained.')
        return save(control, status='paused' if command == 'pause' else 'cancelled', reason='Owner ' + command + ' confirmed; no active call remains.')
    if control['status'] == 'blocked' and snapshot['commandVersion'] == control.get('commandVersion'):
        return control
    control = save(control, command='run', commandVersion=snapshot['commandVersion'], status='running', reason='')
    active = current_attempt(control)
    if active:
        if not active.get('callSid'):
            return save(control, status='blocked', reason='Call creation outcome is unknown; no automatic redial or further call.')
        active = call_status(active, m.provider('/Calls/' + active['callSid'] + '.json'))
        if active['providerStatus'] not in m.TERMINAL:
            return control
    remaining = [target for target in control['targetIds'] if attempt(owner, job, run, target) is None]
    if not remaining:
        return save(control, status='succeeded', reason='Five attempts finished. Completed calls do not prove five interviews or VPN compatibility.')
    now = int(time.time())
    if expiry <= now + MAX_SECONDS + 20:
        return save(control, status='blocked', reason='Approval expires before another bounded call can finish; no call created.')
    local = datetime.now(ZoneInfo('America/Los_Angeles'))
    if not 9 <= local.hour < 17:
        return save(control, status='blocked', reason='Hotel calling is limited to 09:00–17:00 California time; explicitly resume in that window.')
    if cfg.get('enabled') is not True or (r.get(m.VOICE_KEY) or {}).get('activeRequest'):
        return save(control, status='blocked', reason='Calling is disabled or the shared voice lock is held; no call created.')
    target_id = remaining[0]
    target = next(value for value in catalog() if value['id'] == target_id)
    check = {'ConditionCheck': {'TableName': 'live-ninja', 'Key': {'pk': 'USER#' + owner, 'sk': 'JOB#' + job},
        'ConditionExpression': '#version=:version', 'ExpressionAttributeNames': {'#version': 'version'},
        'ExpressionAttributeValues': {':version': main['version']}}}
    try:
        item, fresh = m.reserve(owner, request_id(job, run, target_id), target_id, 'Guest Wi-Fi access research', cfg,
            reserve_amount=RESERVE, target_number=target['phone'], extra_checks=[check], initial_fields={
                'phoneResearchJobId': job, 'phoneResearchRunId': run, 'phoneResearchGeneration': approved['generation'],
                'recordingRunId': os.environ['RUN_ID'], 'scriptTranscript': '', 'reservedUSD': RESERVE})
    except (ValueError, m.ProviderError) as exc:
        reason = str(exc) if isinstance(exc, ValueError) else 'Provider cost lookup failed; no call created.'
        return save(control, status='blocked', reason=reason)
    if not fresh:
        return control
    key = {name: item[name] for name in ('PK', 'SK')}
    item = m.change(key, {'recordingCreateState': 'attempted'}, 'recording_create_attempted', {})
    # Re-read the authoritative command after reserving, before the only POST.
    try:
        current_cfg, _, _, _ = authorize(owner, job, run, {'generation': approved['generation'], 'command': 'run',
            'commandVersion': snapshot['commandVersion'], 'approvalExpiresAt': expiry, 'targetIds': control['targetIds']}, dispatch=True)
        if (current_cfg.get('enabled') is not True or (r.get(m.VOICE_KEY) or {}).get('activeRequest') != item['lockId']
                or item.get('recordingCreateState') != 'attempted' or lifecycle.blocked({'ownerUserId': owner, 'runId': os.environ['RUN_ID']})):
            raise ValueError('Call authorization revoked')
    except ValueError:
        m.change(key, {'providerStatus': 'rejected', 'recordingCreateState': 'rejected'}, 'research_revoked', {}, release=True)
        return save(control, status='blocked', reason='Call authorization changed before dialing.')
    bridge = os.environ.get('INBOUND_BRIDGE_URL', '').rstrip('/')
    if not bridge.startswith('https://'):
        m.change(key, {'providerStatus': 'rejected', 'recordingCreateState': 'rejected'}, 'research_unconfigured', {}, release=True)
        return save(control, status='blocked', reason='Phone bridge is not configured.')
    seconds = min(MAX_SECONDS, int(item.get('budgetExpiresAt', now + MAX_SECONDS + 20)) - int(time.time()) - 20)
    if seconds <= 0:
        m.change(key, {'providerStatus': 'rejected', 'recordingCreateState': 'rejected'}, 'research_expired', {}, release=True)
        return save(control, status='blocked', reason='Daily allowance expires before call connection.')
    import urllib.parse
    callback_url = os.environ['PUBLIC_BASE_URL'].rstrip('/') + '/phone-research/status/' + item['callbackId']
    try:
        result = m.provider('/Calls.json', {'To': target['phone'], 'From': cfg['fromNumber'],
            'Url': bridge + '/voice?' + urllib.parse.urlencode({'run': os.environ['RUN_ID'], 'target': target_id}),
            'Method': 'POST', 'Timeout': 20, 'TimeLimit': seconds,
            'StatusCallback': callback_url, 'StatusCallbackMethod': 'POST', 'StatusCallbackEvent': ['initiated', 'ringing', 'answered', 'completed']})
        sid = result.get('sid', '')
        if not r.SID.fullmatch(sid) or result.get('account_sid') != os.environ['TWILIO_ACCOUNT_SID'] or result.get('status') not in m.STATUSES:
            raise m.ProviderError()
        m.change(key, {'providerStatus': result['status']}, 'research_accepted', {'callSid': sid}, sid=sid)
    except m.ProviderError as exc:
        fields = {'providerStatus': 'rejected' if exc.rejected else 'unknown'}
        if exc.rejected:
            fields['recordingCreateState'] = 'rejected'
        if exc.code:
            fields['providerCode'] = exc.code
        m.change(key, fields, 'research_creation_outcome', fields, release=exc.rejected)
        return save(control, status='blocked', reason='Provider rejected the attempt.' if exc.rejected else 'Provider call creation is ambiguous; no automatic redial.')
    return save(control, status='running', reason='One approved hotel call is active.')


def operation(event):
    owner = event.get('ownerUserId')
    if event.get('operation') not in {'phone_research_catalog', 'phone_research_advance', 'phone_research_status'}:
        return r.response(400)
    try:
        cfg = m.config(owner)
        if not cfg or not lifecycle.owner_active(owner):
            return r.response(403)
        if event.get('operation') == 'phone_research_catalog':
            stopped = cfg.get('enabled') is not True or bool((r.get(m.VOICE_KEY) or {}).get('activeRequest'))
            return r.response(200, {'targets': catalog(), 'verifiedAt': '2026-10-15', 'enabled': not stopped,
                'blockedReason': 'Calling is disabled or stopped.' if stopped else ''})
        job, run = event.get('jobId'), event.get('runId')
        _, _, approved, expiry = authorize(owner, job, run, event if event.get('operation') == 'phone_research_advance' else None)
        key = control_key(owner, job, run)
        control = r.get(key)
        if not control:
            control = save({**key, 'ownerUserId': owner, 'jobId': job, 'researchRunId': run,
                'targetIds': [value['id'] for value in approved['phoneResearch']['targets']], 'generation': approved['generation']}, status='paused', commandVersion=0)
        if event.get('operation') == 'phone_research_advance':
            control = advance(control, event)
        elif event.get('operation') != 'phone_research_status':
            return r.response(400)
        return r.response(200, view(control))
    except (ValueError, TypeError, KeyError, StopIteration):
        return r.response(409, {'error': 'Phone research approval or provider binding could not be verified'})
    except Exception:
        return r.response(503, {'error': 'Phone research is incomplete; refresh without creating another attempt'})


def callback(path, raw, headers):
    data = inbound.signed_form(path, raw, headers)
    match = re.fullmatch(r'/phone-research/status/([0-9a-f]{64})', path)
    if data is None or not match:
        return r.response(403)
    binding = r.get({'PK': 'OWNER_CALLBACK#' + match[1], 'SK': 'META'})
    item = r.get(m.request_key(binding['ownerUserId'], binding['requestId'])) if binding else None
    if not item or not item.get('phoneResearchJobId') or item.get('callbackId') != match[1] or item.get('callSid', data['CallSid']) != data['CallSid']:
        return r.response(403)
    if data.get('To') != item['toNumber'] or data.get('From') != item['fromNumber'] or data.get('CallStatus') not in m.STATUSES:
        return r.response(403)
    owner, job, run = item['ownerUserId'], item['phoneResearchJobId'], item['phoneResearchRunId']
    if lifecycle.blocked({'ownerUserId': owner, 'runId': os.environ['RUN_ID']}):
        return r.response(410)
    item = m.change({key: item[key] for key in ('PK', 'SK')}, {'providerStatus': data['CallStatus']},
        'research_status', {'callSid': data['CallSid'], 'status': data['CallStatus']}, sid=data['CallSid'], release=data['CallStatus'] in m.TERMINAL)
    control = r.get(control_key(owner, job, run))
    if not control:
        return r.response(409)
    if data['CallStatus'] in m.TERMINAL:
        r.event_post(json.dumps({'runId': os.environ['RUN_ID'], 'callSid': data['CallSid'], 'targetId': item['destination'],
            'type': 'call_ended', 'data': {'status': data['CallStatus']}}).encode())
        control = r.get(control_key(owner, job, run))
        try:
            control = advance(control, callback=True)
        except (ValueError, m.ProviderError):
            control = save(control, status='blocked', reason='Approval or provider state changed; explicitly review before resuming.')
    elif control.get('command') in {'pause', 'cancel'} and control['status'] == 'blocked':
        try:
            control = advance(control, callback=True)
        except (ValueError, m.ProviderError):
            control = save(control, status='blocked', reason='Provider termination is unconfirmed; the voice lock is retained.')
    else:
        control = save(control)
    return r.response(200 if notify(control) else 503)


def event_updated(call):
    if not call.get('phoneResearchJobId'):
        return True
    control = r.get(control_key(call['ownerUserId'], call['phoneResearchJobId'], call['phoneResearchRunId']))
    if not control:
        return False
    versions = control.get('evidenceVersions', {})
    if int(versions.get(call['callSid'], 0)) < int(call['version']):
        control = save(control, evidenceVersions={**versions, call['callSid']: int(call['version'])})
    return notify(control)
