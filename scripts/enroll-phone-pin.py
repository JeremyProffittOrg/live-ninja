"""User-run local PIN enrollment. Raw PIN exists in memory only, never in arguments."""
import getpass
import hashlib
import json
from pathlib import Path
import re
import secrets
import subprocess
import sys


def main():
    if sys.argv[1:] not in ([], ['--replace-existing']):
        print('Only --replace-existing is supported. PIN values must be entered at the hidden prompt.')
        return 1
    try:
        import boto3
    except ImportError:
        print('This helper requires Python and the existing boto3 AWS library. Nothing was changed.')
        return 1
    root = Path(__file__).resolve().parents[1]
    result = subprocess.run(['gh', 'repo', 'view', '--json', 'nameWithOwner', '--jq', '.nameWithOwner'],
                            cwd=root, capture_output=True, text=True, creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
    if result.returncode or result.stdout.strip() != 'JeremyProffittOrg/live-ninja':
        print('Repository verification failed. Nothing was changed.')
        return 1
    session = boto3.Session(region_name='us-east-1')
    if session.client('sts').get_caller_identity()['Account'] != '759775734231':
        print('Wrong AWS account. Nothing was changed.')
        return 1
    table = session.resource('dynamodb').Table('live-ninja-phone-trial')
    cfg = table.get_item(Key={'PK': 'CONFIG', 'SK': 'OWNER_MESSAGES'}, ConsistentRead=True).get('Item', {})
    owner = cfg.get('ownerUserId')
    canonical = session.resource('dynamodb').Table('live-ninja').get_item(Key={'pk': 'CONFIG', 'sk': 'OWNER'}, ConsistentRead=True).get('Item', {})
    if not owner or canonical.get('userId') != owner:
        print('Owner account verification failed. Nothing was changed.')
        return 1
    prior = table.get_item(Key={'PK': 'CONFIG', 'SK': 'INBOUND_OWNER'}, ConsistentRead=True).get('Item', {})
    if prior.get('pinHash') and '--replace-existing' not in sys.argv[1:]:
        print('A phone PIN verifier is already enrolled. Skipped; nothing was changed.')
        print('Use --replace-existing only when you intend to replace the existing PIN.')
        return 0
    print('Live Ninja phone PIN enrollment. Repository and AWS account verified.')
    print('Login: https://live.jeremy.ninja/auth/lwa/login')
    print('Stores only a salted PIN verifier in your AWS account. Calling stays disabled.')
    print('Enter six digits. Empty input skips enrollment.')
    if not sys.stdin.isatty():
        print('A local interactive terminal is required. Nothing was changed.')
        return 1
    pin = getpass.getpass('New phone PIN (hidden): ')
    print('Characters entered: ' + str(len(pin)) + '. Length confirms entry, not validity.')
    if not pin:
        print('Skipped. Nothing was changed.')
        return 0
    if not re.fullmatch('[0-9]{6}', pin):
        print('A six digit PIN is required. Nothing was changed.')
        return 1
    salt = secrets.token_bytes(16)
    digest = hashlib.pbkdf2_hmac('sha256', pin.encode(), salt, 600000).hex()
    del pin
    payload = {'operation': 'owner_inbound_enroll', 'ownerUserId': owner, 'pinSalt': salt.hex(), 'pinHash': digest,
               'expectedVersion': int(prior.get('version', 0)), 'replaceExisting': '--replace-existing' in sys.argv[1:]}
    result = session.client('lambda').invoke(FunctionName='live-ninja-phone-trial-recorder',
        InvocationType='RequestResponse', Payload=json.dumps(payload).encode())
    reply = json.loads(result['Payload'].read(4096))
    if result.get('FunctionError') or reply.get('statusCode') != 200:
        print('Enrollment failed. Calling remains disabled.')
        return 1
    body = json.loads(reply['body'])
    if body != {'enrolled': True, 'enabled': False}:
        print('Enrollment result could not be verified. Calling remains disabled.')
        return 1
    print('PIN hash saved. Calling remains disabled; the call stop and routing are unchanged.')
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception:
        print('Enrollment could not be completed. No credential details were logged.')
        raise SystemExit(1)
