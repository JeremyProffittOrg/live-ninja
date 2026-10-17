"""IAM-only account export/deletion for phone data. No scheduled cleanup."""
import base64
import hashlib
import hmac
import json
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request

import recorder as r
import owner_messages as m
import owner_recordings as recordings

MARKER = 'ACCOUNT_DELETION'
PAGE = 25
MAIN_TABLE = 'live-ninja'


class CopyInFlight(Exception):
    def __init__(self, seconds):
        self.seconds = min(120, max(1, seconds))


def owner_key(owner):
    return {'PK': 'OWNER#' + owner, 'SK': MARKER}


def run_key(run):
    return {'PK': 'RUN#' + run, 'SK': MARKER}


def owner_active(owner, deleting=False, since=None):
    import boto3
    table = boto3.resource('dynamodb', region_name='us-east-1').Table(MAIN_TABLE)
    canonical = table.get_item(Key={'pk': 'CONFIG', 'sk': 'OWNER'}, ConsistentRead=True).get('Item', {})
    user = table.get_item(Key={'pk': 'USER#' + owner, 'sk': 'PROFILE'}, ConsistentRead=True).get('Item', {})
    cutoff = int(user.get('tokensValidAfter', 0))
    return (canonical.get('userId') == owner and user.get('role') == 'owner'
            and user.get('status') == ('deleting' if deleting else 'active')
            and (since is None or cutoff <= 0 or cutoff < since))


def barriers(call):
    run = call.get('runId', '')
    owner = call.get('ownerUserId') or (run[len('owner-messages#'):] if run.startswith('owner-messages#') else '')
    return ([owner_key(owner)] if owner else []) + ([run_key(run)] if run else [])


def blocked(call):
    return any(r.get(key) for key in barriers(call))


def ready_conditions(call):
    return [{'ConditionCheck': {'TableName': os.environ['TABLE'], 'Key': key,
            'ConditionExpression': 'attribute_not_exists(PK)'}} for key in barriers(call)]


def delete_objects(prefix, exact=False):
    """Remove every version and delete marker, bounded to one call's prefix."""
    if not re.fullmatch(r'phone-trial/(?:owner-messages#[A-Za-z0-9-]{1,100}|[a-z0-9][a-z0-9-]{0,99})/CA[0-9a-fA-F]{32}/(?:RE[0-9a-fA-F]{32}\.mp3)?', prefix):
        raise ValueError('Invalid recording object boundary')
    s3 = r.clients()[1]
    bucket = os.environ['BUCKET']
    versioned = s3.get_bucket_versioning(Bucket=bucket).get('Status') in {'Enabled', 'Suspended'}
    for _ in range(8):
        if versioned:
            page = s3.list_object_versions(Bucket=bucket, Prefix=prefix, MaxKeys=1000)
            entries = [{'Key': value['Key'], 'VersionId': value['VersionId']} for value in page.get('Versions', []) + page.get('DeleteMarkers', [])]
        else:
            page = s3.list_objects_v2(Bucket=bucket, Prefix=prefix, MaxKeys=1000)
            entries = [{'Key': value['Key']} for value in page.get('Contents', [])]
        if any(not value['Key'].startswith(prefix) or (exact and value['Key'] != prefix) for value in entries):
            raise ValueError('Object listing escaped recording boundary')
        if not entries:
            return
        result = s3.delete_objects(Bucket=bucket, Delete={'Objects': entries, 'Quiet': True})
        if result.get('Errors'):
            raise RuntimeError('Recording object deletion incomplete')
    raise RuntimeError('Recording object cleanup requires retry')


def cleanup_if_deleted(call, key):
    if not blocked(call):
        return False
    expected = f"phone-trial/{call['runId']}/{call['callSid']}/{call['recordingSid']}.mp3"
    if key != expected:
        raise ValueError('Recording object binding mismatch')
    delete_objects(key, exact=True)
    return True


def signed_cursor(owner, sources, index, last):
    raw = json.dumps({'owner': owner, 'sources': sources, 'index': index, 'last': last}, sort_keys=True, separators=(',', ':')).encode()
    encoded = base64.urlsafe_b64encode(raw).decode().rstrip('=')
    return encoded + '.' + r.digest(b'account-export\n' + raw)


def read_cursor(value, owner, sources):
    if not isinstance(value, str) or len(value) > 8192:
        raise ValueError('Invalid export cursor')
    if not value:
        return 0, None
    encoded, signature = value.split('.')
    raw = base64.urlsafe_b64decode(encoded + '=' * (-len(encoded) % 4))
    if not hmac.compare_digest(signature, r.digest(b'account-export\n' + raw)):
        raise ValueError('Invalid export cursor')
    data = json.loads(raw)
    if data.get('owner') != owner or data.get('sources') != sources or type(data.get('index')) is not int or not 0 <= data['index'] < len(sources):
        raise ValueError('Export cursor belongs to another account or source set')
    last = data.get('last')
    if last is not None and (not isinstance(last, dict) or last.get('PK') != sources[data['index']] or not isinstance(last.get('SK'), str)):
        raise ValueError('Invalid export cursor position')
    return data['index'], last


def public_record(row, source):
    # Explicit projection: no PIN verifier, raw numbers, bootstrap, callback IDs,
    # provider credentials, signed links, or future internal fields can leak.
    allowed = ('requestId', 'runId', 'callSid', 'recordingSid', 'targetId', 'destination',
               'providerStatus', 'recordingStatus', 'recordingReason', 'direction',
               'createdAt', 'updatedAt', 'deadline', 'endReason', 'inputTokens',
               'outputTokens', 'audioBytes', 'consented', 'scriptTranscript')
    result = {key: row[key] for key in allowed if key in row}
    def event_data(value):
        if not isinstance(value, dict):
            return {}
        fields = ('type', 'callSid', 'targetId', 'text', 'transcript', 'speaker', 'role', 'summary',
                  'phase', 'action', 'reason', 'status', 'result', 'inputTokens', 'outputTokens', 'duration')
        projected = {key: value[key] for key in fields if key in value and isinstance(value[key], (str, int, float, bool))}
        nested = value.get('data')
        if isinstance(nested, dict):
            projected['data'] = {key: nested[key] for key in fields if key in nested and isinstance(nested[key], (str, int, float, bool))}
        return projected
    if row.get('SK', '').startswith('EVENT#'):
        result['event'] = event_data(row.get('data'))
    if not result:
        return None
    result['kind'] = 'phone_record'
    result['source'] = source
    sid = row.get('callSid', '')
    if row.get('recordingStatus') == 'ready' and r.SID.fullmatch(sid):
        result['downloadPath'] = '/api/v1/phone/recordings/' + sid + '/media?' + urllib.parse.urlencode({'source': source, 'disposition': 'attachment'})
    return result


def export(owner, cfg, event):
    if not owner_active(owner) or r.get(owner_key(owner)):
        return r.response(403)
    runs = recordings.sources(cfg)[1:]
    partitions = ['OWNER#' + owner, 'RUN#' + m.namespace(owner)] + ['RUN#' + run for run in runs]
    index, last = read_cursor(event.get('cursor', ''), owner, partitions)
    args = {'KeyConditionExpression': 'PK = :pk', 'ExpressionAttributeValues': {':pk': partitions[index]}, 'ConsistentRead': True, 'Limit': PAGE}
    if last:
        args['ExclusiveStartKey'] = last
    page = r.clients()[0].query(**args)
    source = 'messages' if index < 2 else runs[index - 2]
    items = []
    for row in page.get('Items', []):
        if row.get('PK') != partitions[index] or (index < 2 and row.get('ownerUserId', owner) != owner):
            raise ValueError('Export row belongs to another account')
        projected = public_record(row, source)
        if projected:
            items.append(projected)
    last = page.get('LastEvaluatedKey')
    if not last:
        index += 1
    cursor = signed_cursor(owner, partitions, index, last) if index < len(partitions) else ''
    return r.response(200, {'items': items, 'nextCursor': cursor, 'complete': not cursor})


def provider_delete(sid, recording):
    """Confirm provider identity before a DELETE; a prior successful delete is OK."""
    try:
        info = m.provider('/Recordings/' + recording + '.json')
    except m.ProviderError as exc:
        if exc.code == 20404:
            return
        raise
    account = os.environ['TWILIO_ACCOUNT_SID']
    if info.get('sid') != recording or info.get('call_sid') != sid or info.get('account_sid') != account:
        raise ValueError('Provider recording binding mismatch')
    key = os.environ.get('TWILIO_API_KEY_SID') or account
    secret = os.environ.get('TWILIO_API_KEY_SECRET') or os.environ['TWILIO_AUTH_TOKEN']
    authorization = base64.b64encode((key + ':' + secret).encode()).decode()
    request = urllib.request.Request(f'https://api.twilio.com/2010-04-01/Accounts/{account}/Recordings/{recording}.json', method='DELETE', headers={'Authorization': 'Basic ' + authorization})
    try:
        with urllib.request.build_opener(r.NoRedirect).open(request, timeout=10) as result:
            if result.status != 204:
                raise RuntimeError('Provider recording deletion unconfirmed')
    except urllib.error.HTTPError as exc:
        if exc.code != 404:
            raise RuntimeError('Provider recording deletion failed') from None


def purge_call(row):
    sid, run = row.get('callSid', ''), row.get('runId', '')
    if not r.SID.fullmatch(sid) or row.get('PK') != 'RUN#' + run or row.get('SK') != 'CALL#' + sid:
        raise ValueError('Invalid call journal identity')
    if row.get('recordingStatus') == 'copying' and int(row.get('copyLeaseUntil', 0)) > int(time.time()):
        raise CopyInFlight(int(row['copyLeaseUntil']) - int(time.time()))
    try:
        provider_call = m.provider('/Calls/' + sid + '.json')
    except m.ProviderError as exc:
        if exc.code != 20404:
            raise
        provider_call = None
    if provider_call is not None and (provider_call.get('sid') != sid or provider_call.get('account_sid') != os.environ['TWILIO_ACCOUNT_SID']):
        raise ValueError('Provider call binding mismatch')
    if provider_call is not None and provider_call.get('status') not in m.TERMINAL:
        provider_call = m.provider('/Calls/' + sid + '.json', {'Status': 'completed'})
        if provider_call.get('sid') != sid or provider_call.get('account_sid') != os.environ['TWILIO_ACCOUNT_SID'] or provider_call.get('status') not in m.TERMINAL:
            raise RuntimeError('Provider call termination unconfirmed')
    # The per-call provider listing also covers a recording whose create reply
    # was lost before its RecordingSid reached our journal.
    path = '/Calls/' + sid + '/Recordings.json?PageSize=100'
    recordings_found = set()
    for _ in range(5):
        if provider_call is None:
            break
        page = m.provider(path)
        for value in page.get('recordings', []):
            recording = value.get('sid', '')
            if not r.REC.fullmatch(recording) or value.get('call_sid') != sid or value.get('account_sid') != os.environ['TWILIO_ACCOUNT_SID']:
                raise ValueError('Provider call recording mismatch')
            recordings_found.add(recording)
        next_page = page.get('next_page_uri')
        if not next_page:
            break
        expected = '/2010-04-01/Accounts/' + os.environ['TWILIO_ACCOUNT_SID']
        if not isinstance(next_page, str) or not next_page.startswith(expected + '/Calls/' + sid + '/Recordings.json?'):
            raise ValueError('Provider recording page escaped call')
        path = next_page[len(expected):]
    else:
        raise RuntimeError('Provider recording listing exceeds bound')
    known = row.get('recordingSid')
    if known:
        if not r.REC.fullmatch(known):
            raise ValueError('Invalid recording identifier')
        recordings_found.add(known)
    for recording in recordings_found:
        provider_delete(sid, recording)
        link = r.get({'PK': 'LINK#' + recording, 'SK': 'META'})
        if link:
            if link.get('runId') != run or link.get('callSid') != sid:
                raise ValueError('Recording link belongs to another call')
            token = r.digest(('recording-link\n' + run + '\n' + sid + '\n' + recording + '\n' + str(link['issuedAt'])).encode())
            r.clients()[0].delete_item(Key={'PK': 'TOKEN#' + hashlib.sha256(token.encode()).hexdigest(), 'SK': 'META'})
            r.clients()[0].delete_item(Key={'PK': 'LINK#' + recording, 'SK': 'META'})
    delete_objects(f'phone-trial/{run}/{sid}/')


def marker(owner, cfg):
    current = r.get(owner_key(owner))
    if current:
        return current
    if not owner_active(owner, deleting=True):
        raise ValueError('Account deletion was not authenticated')
    runs = [m.namespace(owner)] + recordings.sources(cfg)[1:]
    item = {**owner_key(owner), 'ownerUserId': owner, 'runs': runs, 'phase': 0, 'version': 1, 'createdAt': int(time.time())}
    # Fence every known recording namespace before touching any audio.
    operations = [m.put(item)]
    for run in runs:
        operations.append(m.put({**run_key(run), 'ownerUserId': owner, 'version': 1}))
    operations.append({'Update': {'TableName': os.environ['TABLE'], 'Key': m.CONFIG_KEY,
        'UpdateExpression': 'SET #enabled=:no', 'ConditionExpression': 'ownerUserId=:owner',
        'ExpressionAttributeNames': {'#enabled': 'enabled'}, 'ExpressionAttributeValues': {':no': False, ':owner': owner}}})
    try:
        m.transact(operations)
    except Exception as exc:
        if not r.conditional(exc):
            raise
        current = r.get(owner_key(owner))
        if not current:
            raise ValueError('Recording deletion ownership changed')
        return current
    return item


def purge(owner, cfg):
    state = marker(owner, cfg)
    if state.get('ownerUserId') != owner:
        raise ValueError('Invalid deletion owner')
    partitions = ['RUN#' + run for run in state['runs']] + ['OWNER#' + owner]
    phase = int(state['phase'])
    if phase >= len(partitions):
        return r.response(200, {'complete': True})
    pk = partitions[phase]
    page = r.clients()[0].query(KeyConditionExpression='PK = :pk', ExpressionAttributeValues={':pk': pk}, ConsistentRead=True, Limit=PAGE)
    rows = [row for row in page.get('Items', []) if row.get('SK') != MARKER]
    for row in rows:
        if row.get('PK') != pk or row.get('ownerUserId', owner) != owner:
            raise ValueError('Deletion row belongs to another account')
        if row.get('SK', '').startswith('CALL#'):
            purge_call(row)
        if row.get('SK', '').startswith('MESSAGE#'):
            if not row.get('callSid') and row.get('providerStatus') in {'creating', 'unknown'}:
                raise RuntimeError('Unresolved provider creation must be reconciled before deletion')
            lock = r.get(m.VOICE_KEY) or {}
            expected_lock = 'owner:' + owner + ':' + str(row.get('requestId', ''))
            if row.get('lockId') == expected_lock and lock.get('activeRequest') == expected_lock:
                sid = row.get('callSid', '')
                if not r.SID.fullmatch(sid):
                    raise RuntimeError('Unresolved call lock cannot be released')
                try:
                    provider_call = m.provider('/Calls/' + sid + '.json')
                    if provider_call.get('sid') != sid or provider_call.get('account_sid') != os.environ['TWILIO_ACCOUNT_SID'] or provider_call.get('status') not in m.TERMINAL:
                        raise RuntimeError('Call lock terminal state is unconfirmed')
                except m.ProviderError as exc:
                    if exc.code != 20404:
                        raise
                r.clients()[0].update_item(Key=m.VOICE_KEY, UpdateExpression='REMOVE activeRequest',
                    ConditionExpression='activeRequest=:request', ExpressionAttributeValues={':request': expected_lock})
            callback = row.get('callbackId', '')
            if callback:
                if not isinstance(callback, str) or not re.fullmatch('[0-9a-f]{64}', callback):
                    raise ValueError('Invalid callback identity')
                binding = r.get({'PK': 'OWNER_CALLBACK#' + callback, 'SK': 'META'})
                if binding and (binding.get('ownerUserId') != owner or binding.get('requestId') != row.get('requestId')):
                    raise ValueError('Callback ownership mismatch')
                if binding:
                    r.clients()[0].delete_item(Key={'PK': 'OWNER_CALLBACK#' + callback, 'SK': 'META'})
        r.clients()[0].delete_item(Key={'PK': pk, 'SK': row['SK']})
        if row.get('SK', '').startswith('CALL#'):
            # Provider calls consume most of the recorder's 60-second runtime.
            # Persist one completed call per invocation, then let the worker resume.
            return r.response(200, {'complete': False})
    if rows or page.get('LastEvaluatedKey'):
        return r.response(200, {'complete': False})
    if phase == len(partitions) - 1:
        # Remove private destination/PIN configuration only after all data is gone.
        for key in (m.CONFIG_KEY, {'PK': 'CONFIG', 'SK': 'INBOUND_OWNER'}):
            value = r.get(key)
            if value and value.get('ownerUserId') == owner:
                r.clients()[0].delete_item(Key=key, ConditionExpression='ownerUserId=:owner', ExpressionAttributeValues={':owner': owner})
    next_state = {**state, 'phase': phase + 1, 'complete': phase + 1 == len(partitions), 'version': int(state['version']) + 1}
    r.commit(state, next_state)
    return r.response(200, {'complete': phase + 1 == len(partitions)})


def operation(event):
    owner = event.get('ownerUserId', '')
    if not isinstance(owner, str) or not re.fullmatch('[A-Za-z0-9-]{1,100}', owner):
        return r.response(400)
    cfg = r.get(m.CONFIG_KEY)
    deletion = r.get(owner_key(owner))
    if (not cfg or cfg.get('ownerUserId') != owner) and not deletion:
        return r.response(200, {'items': [], 'complete': True, 'nextCursor': ''})
    try:
        if event.get('operation') == 'owner_account_export':
            return export(owner, cfg, event)
        if event.get('operation') == 'owner_account_purge':
            return purge(owner, cfg)
        return r.response(400)
    except CopyInFlight as exc:
        return r.response(503, {'error': 'Recording copy is still in flight', 'retryAfterSeconds': exc.seconds})
    except (ValueError, TypeError, KeyError):
        return r.response(409, {'error': 'Phone account data binding could not be verified'})
    except Exception:
        return r.response(503, {'error': 'Phone account operation incomplete; retry required'})
