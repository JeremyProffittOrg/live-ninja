"""Disabled-by-default inbound owner conversations. Never log callbacks or PINs."""
import base64
import hashlib
import hmac
import html
import json
import os
import re
import time
import urllib.parse
import urllib.request
from decimal import Decimal

import recorder as r
import owner_messages as m

CONFIG = {"PK": "CONFIG", "SK": "INBOUND_OWNER"}
MAX_SECONDS = 180
RESERVE = Decimal("1.40")
PIN_ITERATIONS = 600000
APP = "https://live.jeremy.ninja"
BROKER = "live-ninja-realtime-broker"


def xml(body):
    return {"statusCode": 200, "headers": {"content-type": "application/xml", "cache-control": "no-store"},
            "body": '<Response>' + body + '</Response>'}


def bye():
    return xml('<Say voice="Polly.Joanna-Neural">This phone session is not available. Goodbye.</Say><Hangup/>')


def base():
    return os.environ["PUBLIC_BASE_URL"].rstrip('/') + '/inbound-owner/'


def valid_config(cfg, inbound, since=None):
    import account_lifecycle
    return bool(cfg and cfg.get('enabled') is True and inbound and inbound.get('enabled') is True
                and inbound.get('ownerUserId') == cfg.get('ownerUserId')
                and re.fullmatch('[0-9a-f]{64}', inbound.get('pinHash', ''))
                and re.fullmatch('[0-9a-f]{32}', inbound.get('pinSalt', ''))
                and inbound.get('pinIterations') == PIN_ITERATIONS
                and not r.get(account_lifecycle.owner_key(cfg['ownerUserId']))
                and account_lifecycle.owner_active(cfg['ownerUserId'], since=since))


def signed_form(path, raw, headers):
    pairs = urllib.parse.parse_qs(raw.decode(), keep_blank_values=True, max_num_fields=40)
    signed = os.environ['PUBLIC_BASE_URL'].rstrip('/') + path
    for key in sorted(pairs):
        for value in sorted(set(pairs[key])):
            signed += key + value
    expected = base64.b64encode(hmac.new(os.environ['TWILIO_AUTH_TOKEN'].encode(), signed.encode(), hashlib.sha1).digest()).decode()
    if not hmac.compare_digest(expected, headers.get('x-twilio-signature', '')) or any(len(v) != 1 for v in pairs.values()):
        return None
    data = {k: v[0] for k, v in pairs.items()}
    if data.get('AccountSid') != os.environ['TWILIO_ACCOUNT_SID'] or not r.SID.fullmatch(data.get('CallSid', '')):
        return None
    return data


def transition(item, **fields):
    new = {**item, **fields, 'version': int(item['version']) + 1, 'updatedAt': int(time.time())}
    r.commit(item, new)
    return new


def gather(item, consent=False):
    step = 'consent' if consent else 'pin/' + str(item.get('pinAttempts', 0) + 1)
    action = html.escape(base() + step + '/' + item['callbackId'], quote=True)
    prompt = 'Press 1 to allow recording this conversation, or any other key to end.' if consent else 'Enter your six digit Live Ninja PIN.'
    return xml(f'<Gather input="dtmf" numDigits="{1 if consent else 6}" timeout="6" actionOnEmptyResult="true" method="POST" action="{action}"><Say voice="Polly.Joanna-Neural">{prompt}</Say></Gather><Hangup/>')


def final_transcript(item):
    bootstrap = item.get('bootstrap') or {}
    if not bootstrap.get('bridgeToken'):
        return
    body = json.dumps({'sessionId': bootstrap['sessionId'], 'final': True, 'turns': []}).encode()
    request = urllib.request.Request(APP + '/api/v1/transcript', data=body,
        headers={'Authorization': 'Bearer ' + bootstrap['bridgeToken'], 'Content-Type': 'application/json'})
    with urllib.request.build_opener(r.NoRedirect).open(request, timeout=8) as result:
        result.read(4096)


def end(item, reason, hangup=False, finalized=False, provider_status=None):
    # Do not release a paid-call lock until the provider has confirmed termination.
    provider_confirmed = not hangup
    if hangup:
        try:
            result = m.provider('/Calls/' + item['callSid'] + '.json', {'Status': 'completed'})
            provider_confirmed = (result.get('sid') == item['callSid']
                                  and result.get('account_sid') == os.environ['TWILIO_ACCOUNT_SID']
                                  and result.get('status') in m.TERMINAL)
            if provider_confirmed:
                provider_status = result['status']
        except Exception:
            provider_confirmed = False
    finalized = finalized or item.get('transcriptFinalized') is True
    if not finalized:
        try:
            final_transcript(item)
            finalized = True
        except Exception:
            # Keep the failure visible; the scoped session also expires server-side.
            reason += '_transcript_cleanup_failed'
    key = {k: item[k] for k in ('PK', 'SK')}
    if not provider_confirmed:
        # Broker cleanup is independent of telephone termination. Preserve the
        # unresolved provider state and lock, but do not leak the owner session.
        m.change(key, {'transcriptFinalized': finalized, 'endReason': reason + '_provider_cleanup_unconfirmed'},
                 'inbound_cleanup_pending', {'reason': reason, 'transcriptFinalized': finalized}, sid=item['callSid'])
        return False
    m.change(key, {'inboundState': 'ended', 'providerStatus': provider_status or item.get('providerStatus', 'unknown'),
                  'endReason': reason, 'transcriptFinalized': finalized},
             'inbound_ended', {'reason': reason, 'callSid': item['callSid']}, sid=item['callSid'], release=True)
    call = r.get(r.call_key(item['callSid'], m.namespace(item['ownerUserId'])))
    if call and call.get('recordingStatus') == 'pending' and not call.get('recordingSid'):
        try:
            unresolved = call.get('recordingCreateState') == 'attempted'
            r.commit(call, {**call, 'version': int(call['version']) + 1, 'recordingStatus': 'failed' if unresolved else 'absent',
                            'recordingReason': 'recording_create_unknown' if unresolved else reason})
        except Exception as exc:
            if not r.conditional(exc):
                raise
    return True


def start(data):
    cfg, inbound = r.get(m.CONFIG_KEY), r.get(CONFIG)
    if not valid_config(cfg, inbound, since=int(time.time())) or (r.get(m.VOICE_KEY) or {}).get('activeRequest'):
        return bye()
    bridge = os.environ.get('INBOUND_BRIDGE_URL', '').rstrip('/')
    parsed = urllib.parse.urlsplit(bridge)
    if parsed.scheme != 'https' or not parsed.hostname or parsed.path or parsed.query or parsed.fragment or parsed.username:
        return bye()
    alias = next((alias for alias, number in cfg.get('destinations', {}).items() if number == data.get('From')), None)
    if alias not in {'primary', 'secondary'} or data.get('To') != cfg.get('fromNumber') or data.get('Direction') != 'inbound':
        return bye()
    sid = data['CallSid']
    now = int(time.time())
    check = {'ConditionCheck': {'TableName': os.environ['TABLE'], 'Key': CONFIG,
        'ConditionExpression': '#enabled=:yes AND ownerUserId=:owner AND pinHash=:hash AND pinSalt=:salt',
        'ExpressionAttributeNames': {'#enabled': 'enabled'},
        'ExpressionAttributeValues': {':yes': True, ':owner': cfg['ownerUserId'], ':hash': inbound['pinHash'], ':salt': inbound['pinSalt']}}}
    try:
        item, fresh = m.reserve(cfg['ownerUserId'], 'inbound-' + sid, alias, 'Inbound owner conversation', cfg,
            reserve_amount=RESERVE, initial_fields={'direction': 'inbound', 'callSid': sid, 'inboundState': 'auth',
            'pinAttempts': 0, 'deadline': now + MAX_SECONDS, 'authDeadline': now + 30, 'bridge': bridge,
            'scriptTranscript': '', 'reservedUSD': RESERVE}, extra_checks=[check])
    except (ValueError, m.ProviderError):
        return bye()
    if not fresh:
        return bye()
    item = m.change({k: item[k] for k in ('PK', 'SK')}, {}, 'inbound_registered', {'callSid': sid}, sid=sid)
    deadline = min(item['deadline'], int(item.get('budgetExpiresAt', item['deadline'])))
    if deadline - now < 30:
        end(item, 'budget_expired', hangup=True)
        return bye()
    item = transition(item, deadline=deadline)
    try:
        # Provider hard cap applies even if the bridge or a callback crashes.
        remaining = deadline - int(time.time())
        if remaining <= 0:
            raise ValueError('Admission expired')
        result = m.provider('/Calls/' + sid + '.json', {'TimeLimit': remaining,
                   'StatusCallback': base() + 'status/' + item['callbackId'], 'StatusCallbackMethod': 'POST'})
        if result.get('sid') != sid or result.get('account_sid') != os.environ['TWILIO_ACCOUNT_SID'] or result.get('status') not in m.STATUSES:
            raise ValueError('Provider cap binding unavailable')
    except Exception:
        end(item, 'provider_cap_failed', hangup=True)
        return bye()
    return gather(item)


def mint(item):
    import boto3
    result = boto3.client('lambda').invoke(FunctionName=BROKER, InvocationType='RequestResponse',
        Payload=json.dumps({'mode': 'phone-session', 'userId': item['ownerUserId'], 'surface': 'phone',
                            'phoneCallSid': item['callSid'], 'phoneDeadline': int(item['deadline'])}).encode())
    raw = result['Payload'].read(200001)
    if result.get('FunctionError') or len(raw) > 200000:
        raise ValueError('Broker unavailable')
    value = json.loads(raw)
    if value.get('mode') != 'phone' or not value.get('bridgeToken') or not value.get('sessionId') or not isinstance(value.get('sessionConfig'), dict):
        raise ValueError('Invalid broker bootstrap')
    return value


def callback(path, raw, headers):
    data = signed_form(path, raw, headers)
    if data is None:
        return r.response(403)
    if path == '/inbound-owner/voice':
        return start(data)
    match = re.fullmatch(r'/inbound-owner/(pin/[123]|consent|status|recording|ended)/([0-9a-f]{64})', path)
    if not match:
        return r.response(404)
    kind, token = match.groups()
    binding = r.get({'PK': 'OWNER_CALLBACK#' + token, 'SK': 'META'})
    item = r.get(m.request_key(binding['ownerUserId'], binding['requestId'])) if binding else None
    if not item or item.get('direction') != 'inbound' or item.get('callSid') != data['CallSid'] or item.get('callbackId') != token:
        return r.response(403)
    if any(data.get(field) is not None and data[field] != item[stored] for field, stored in (('From', 'toNumber'), ('To', 'fromNumber'))):
        return r.response(403)
    if kind == 'recording':
        recording = data.get('RecordingSid', '')
        if not r.REC.fullmatch(recording) or not item.get('consented'):
            return r.response(403)
        import account_lifecycle
        call = r.get(r.call_key(item['callSid'], m.namespace(item['ownerUserId'])))
        if call and account_lifecycle.blocked(call):
            return account_lifecycle.resolve_deleted_recording(call, recording, data.get('RecordingStatus'))
        return r.ingest_recording(item['callSid'], recording, data.get('RecordingStatus'), run_id=m.namespace(item['ownerUserId']))
    if item.get('inboundState') == 'ended':
        return r.response(200) if kind == 'status' else bye()
    if kind in {'status', 'ended'}:
        if kind == 'ended' or data.get('CallStatus') in m.TERMINAL:
            end(item, 'provider_terminal', hangup=kind == 'ended', provider_status=data.get('CallStatus'))
        return bye() if kind == 'ended' else r.response(200)
    cfg, inbound = r.get(m.CONFIG_KEY), r.get(CONFIG)
    lock = r.get(m.VOICE_KEY) or {}
    if not valid_config(cfg, inbound, since=item['createdAt']) or lock.get('activeRequest') != item['lockId'] or int(time.time()) >= item['deadline']:
        end(item, 'admission_revoked', hangup=True)
        return bye()
    if kind.startswith('pin/'):
        attempt = int(kind[-1])
        if item.get('inboundState') != 'auth' or attempt != item.get('pinAttempts', 0) + 1 or int(time.time()) >= item['authDeadline']:
            return bye()
        item = transition(item, pinAttempts=attempt, inboundState='pin_check')
        digits = data.get('Digits', '')
        valid = bool(re.fullmatch('[0-9]{6}', digits)) and hmac.compare_digest(
            hashlib.pbkdf2_hmac('sha256', digits.encode(), bytes.fromhex(inbound['pinSalt']), PIN_ITERATIONS).hex(), inbound['pinHash'])
        if not valid:
            if attempt == 3:
                end(item, 'pin_rejected', hangup=True)
                return bye()
            item = transition(item, inboundState='auth')
            return gather(item)
        item = transition(item, inboundState='consent')
        item = m.change({k: item[k] for k in ('PK', 'SK')}, {}, 'inbound_authenticated',
                        {'callSid': item['callSid'], 'attempts': attempt}, sid=item['callSid'])
        return gather(item, consent=True)
    if item.get('inboundState') != 'consent':
        return bye()
    if data.get('Digits') != '1':
        end(item, 'consent_declined', hangup=True)
        return bye()
    # Persist before invoking: an ambiguous mint/recording creation is never retried.
    item = transition(item, inboundState='minting', consented=True)
    bootstrap = None
    try:
        call = r.get(r.call_key(item['callSid'], m.namespace(item['ownerUserId'])))
        r.commit(call, {**call, 'version': int(call['version']) + 1, 'consentStatus': 'granted'})
        bootstrap = mint(item)
        item = transition(item, bootstrap=bootstrap, inboundState='starting')
        run_id = m.namespace(item['ownerUserId'])
        r.recording_creation(item['callSid'], 'attempted', run_id)
        try:
            rec = m.provider('/Calls/' + item['callSid'] + '/Recordings.json', {'RecordingTrack': 'both', 'RecordingChannels': 'dual',
                'RecordingStatusCallback': base() + 'recording/' + token, 'RecordingStatusCallbackMethod': 'POST',
                'RecordingStatusCallbackEvent': ['completed', 'absent']})
        except m.ProviderError as exc:
            if exc.rejected:
                r.recording_creation(item['callSid'], 'rejected', run_id)
            raise
        if not r.REC.fullmatch(rec.get('sid', '')) or rec.get('call_sid') != item['callSid'] or rec.get('account_sid') != os.environ['TWILIO_ACCOUNT_SID']:
            raise ValueError('Invalid recording binding')
        r.set_recording(item['callSid'], rec['sid'], 'recording', run_id=run_id, recordingCreateState='confirmed')
        item = transition(item, inboundState='ready')
    except Exception:
        # A concurrent hangup/deletion can reject persistence after mint returns.
        # Retain that known capability only for finalizing its exact session.
        if bootstrap is not None:
            item = {**item, 'bootstrap': bootstrap}
        end(item, 'bootstrap_failed', hangup=True)
        return bye()
    stream = item['bridge'].replace('https://', 'wss://', 1) + '/owner-media'
    return xml('<Say voice="Polly.Joanna-Neural">Recording starts now.</Say><Connect method="POST" action="' +
        html.escape(base() + 'ended/' + token, quote=True) + '"><Stream url="' + html.escape(stream, quote=True) +
        '"><Parameter name="capability" value="' + token + '"/></Stream></Connect><Hangup/>')


def redeem(raw, headers, action='redeem'):
    if action not in {'redeem', 'check', 'finish'}:
        return r.response(404)
    if not hmac.compare_digest(r.digest(('inbound-owner/' + action + '\n').encode() + raw), headers.get('x-ln-signature', '')):
        return r.response(403)
    data = json.loads(raw)
    token, sid = data.get('capability', ''), data.get('callSid', '')
    if not re.fullmatch('[0-9a-f]{64}', token) or not r.SID.fullmatch(sid):
        return r.response(400)
    binding = r.get({'PK': 'OWNER_CALLBACK#' + token, 'SK': 'META'})
    item = r.get(m.request_key(binding['ownerUserId'], binding['requestId'])) if binding else None
    if not item or item.get('direction') != 'inbound' or item.get('callSid') != sid:
        return r.response(403)
    if action == 'finish':
        reason = data.get('reason', 'bridge_finished')
        if not isinstance(reason, str) or not re.fullmatch('[a-z_]{1,60}', reason):
            return r.response(400)
        usage = {key: data.get(key, 0) for key in ('inputTokens', 'outputTokens')}
        if any(type(value) is not int or not 0 <= value <= 100000 for value in usage.values()):
            return r.response(400)
        key = {k: item[k] for k in ('PK', 'SK')}
        item = m.change(key, usage, 'inbound_usage', usage, sid=sid)
        return r.response(200 if end(item, reason, hangup=True,
                                    finalized=data.get('transcriptFinalized') is True) else 503)
    required_state = 'ready' if action == 'redeem' else 'redeemed'
    if item.get('inboundState') != required_state or int(time.time()) >= item['deadline']:
        return r.response(403)
    if not valid_config(r.get(m.CONFIG_KEY), r.get(CONFIG), since=item['createdAt']) or (r.get(m.VOICE_KEY) or {}).get('activeRequest') != item['lockId']:
        return r.response(403)
    if action == 'check':
        return r.response(200)
    try:
        item = transition(item, inboundState='redeemed')
    except Exception as exc:
        if not r.conditional(exc):
            raise
        return r.response(409)
    return r.response(200, {**item['bootstrap'], 'callSid': sid, 'deadline': item['deadline']})


def enroll(event):
    """IAM-only salted verifier enrollment. Always leaves inbound calling disabled."""
    cfg = r.get(m.CONFIG_KEY)
    if not cfg or cfg.get('ownerUserId') != event.get('ownerUserId'):
        return r.response(403)
    import account_lifecycle
    if r.get(account_lifecycle.owner_key(cfg['ownerUserId'])) or not account_lifecycle.owner_active(cfg['ownerUserId']):
        return r.response(403)
    if not re.fullmatch('[0-9a-f]{64}', event.get('pinHash', '')) or not re.fullmatch('[0-9a-f]{32}', event.get('pinSalt', '')):
        return r.response(400)
    old = r.get(CONFIG)
    if event.get('expectedVersion') != int((old or {}).get('version', 0)):
        return r.response(409, {'error': 'Enrollment changed; restart the local helper'})
    if old and old.get('pinHash') and event.get('replaceExisting') is not True:
        return r.response(409, {'error': 'Existing enrollment was not selected for replacement'})
    new = {**CONFIG, 'version': int((old or {}).get('version', 0)) + 1, 'ownerUserId': cfg['ownerUserId'],
           'enabled': False, 'pinHash': event['pinHash'], 'pinSalt': event['pinSalt'], 'pinIterations': PIN_ITERATIONS}
    try:
        m.transact([m.put(new, old), *account_lifecycle.ready_conditions({'ownerUserId': cfg['ownerUserId']})])
    except Exception as exc:
        if not r.conditional(exc):
            raise
        return r.response(409, {'error': 'Enrollment changed; restart the local helper'})
    return r.response(200, {'enrolled': True, 'enabled': False})
