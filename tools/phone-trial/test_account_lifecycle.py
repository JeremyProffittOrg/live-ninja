import copy
import json
import types
import sys
import unittest
from unittest.mock import Mock, patch

import account_lifecycle as a
import owner_messages as m
import recorder as r
import test_recorder as fixtures
from test_recorder import ACCOUNT, CALL, REC, Conflict

OWNER = '11111111-1111-4111-8111-111111111111'
OTHER = '22222222-2222-4222-8222-222222222222'
REAL_COMMIT = r.commit
REAL_OWNER_ACTIVE = a.owner_active
REAL_PROVIDER_DELETE = a.provider_delete


class AccountTable(fixtures.Table):
    def __init__(self):
        super().__init__()
        self.meta = types.SimpleNamespace(client=types.SimpleNamespace(transact_write_items=self.transact))

    def query(self, **args):
        pk = args['ExpressionAttributeValues'][':pk']
        rows = sorted((copy.deepcopy(value) for (key, _), value in self.items.items() if key == pk), key=lambda value: value['SK'])
        last = args.get('ExclusiveStartKey', {}).get('SK', '')
        rows = [row for row in rows if row['SK'] > last]
        limit = args.get('Limit', len(rows))
        page = {'Items': rows[:limit]}
        if len(rows) > limit:
            page['LastEvaluatedKey'] = {key: rows[limit-1][key] for key in ('PK', 'SK')}
        return page

    def delete_item(self, Key, **args):
        if args and self.items.get((Key['PK'], Key['SK']), {}).get('ownerUserId') != args['ExpressionAttributeValues'][':owner']:
            raise Conflict()
        self.items.pop((Key['PK'], Key['SK']), None)

    def update_item(self, Key, ExpressionAttributeValues, **_):
        item = self.items[(Key['PK'], Key['SK'])]
        if item.get('activeRequest') != ExpressionAttributeValues[':request']:
            raise Conflict()
        item.pop('activeRequest')

    def transact(self, TransactItems):
        before = copy.deepcopy(self.items)
        try:
            for operation in TransactItems:
                kind, value = next(iter(operation.items()))
                key = value.get('Key') or {name: value['Item'][name] for name in ('PK', 'SK')}
                old = r.get(key)
                condition = value.get('ConditionExpression', '').replace(' ', '')
                if condition == 'attribute_not_exists(PK)' and old:
                    raise Conflict()
                if condition == '#v=:v' and (old or {}).get('version') != value['ExpressionAttributeValues'][':v']:
                    raise Conflict()
                if condition == 'ownerUserId=:owner' and (old or {}).get('ownerUserId') != value['ExpressionAttributeValues'][':owner']:
                    raise Conflict()
                if kind == 'Put':
                    self.put_item(Item=value['Item'])
                elif kind == 'Update':
                    self.put_item(Item={**old, 'enabled': value['ExpressionAttributeValues'][':no']})
        except Exception:
            self.items = before
            raise


class AccountS3:
    def __init__(self):
        self.versions = []
        self.deleted = []
        self.errors = False

    def get_bucket_versioning(self, **_):
        return {'Status': 'Enabled'}

    def list_object_versions(self, Prefix, **_):
        return {'Versions': [copy.deepcopy(item) for item in self.versions if item['Key'].startswith(Prefix)]}

    def delete_objects(self, Delete, **_):
        if self.errors:
            return {'Errors': [{'Code': 'AccessDenied'}]}
        for item in Delete['Objects']:
            self.deleted.append(item)
            self.versions.remove(item)
        return {}


class PhoneAccountTests(unittest.TestCase):
    commit = fixtures.RecorderTests.commit

    def setUp(self):
        fixtures.RecorderTests.setUp(self)
        r._table, r._s3 = AccountTable(), AccountS3()
        self.active = patch.object(a, 'owner_active', return_value=True)
        self.active_mock = self.active.start()
        self.addCleanup(self.active.stop)
        self.provider_patch = patch.object(m, 'provider', self.provider)
        self.provider_patch.start(); self.addCleanup(self.provider_patch.stop)
        self.deleted_provider = []
        self.delete_patch = patch.object(a, 'provider_delete', lambda sid, rec: self.deleted_provider.append((sid, rec)))
        self.delete_patch.start(); self.addCleanup(self.delete_patch.stop)
        self.cfg = {**m.CONFIG_KEY, 'ownerUserId': OWNER, 'enabled': True, 'recordingRuns': ['trial'],
                    'fromNumber': '+12025550100', 'destinations': {'primary': '+12025550101'}}
        r._table.put_item(Item=self.cfg)
        r._table.put_item(Item={'PK': 'CONFIG', 'SK': 'INBOUND_OWNER', 'ownerUserId': OWNER, 'pinHash': 'private-pin-hash'})

    def tearDown(self):
        fixtures.RecorderTests.tearDown(self)

    def provider(self, path, form=None):
        if '/Recordings.json?' in path:
            return {'recordings': [{'sid': REC, 'call_sid': CALL, 'account_sid': ACCOUNT}]}
        return {'sid': CALL, 'account_sid': ACCOUNT, 'status': 'completed'}

    def call(self, run='trial', **fields):
        item = {**r.call_key(CALL, run), 'runId': run, 'callSid': CALL, 'recordingSid': REC,
                'recordingStatus': 'ready', 'version': 1, 's3Key': f'phone-trial/{run}/{CALL}/{REC}.mp3', **fields}
        r._table.put_item(Item=item)
        return item

    def invoke(self, operation='owner_account_purge', **fields):
        return a.operation({'operation': operation, 'ownerUserId': OWNER, **fields})

    def test_export_pages_are_owner_bound_and_strip_internal_secrets(self):
        for index in range(30):
            r._table.put_item(Item={**m.request_key(OWNER, f'req-{index:02}'), 'ownerUserId': OWNER,
                'requestId': f'req-{index:02}', 'scriptTranscript': 'hello', 'fromNumber': '+12025550100',
                'pinHash': 'secret-hash', 'callbackId': 'secret-capability', 'bootstrap': {'bridgeToken': 'secret-token'}})
        self.call()
        response = self.invoke('owner_account_export')
        first = json.loads(response['body']); self.assertEqual(len(first['items']), 25)
        cursor = first['nextCursor']; self.assertTrue(cursor)
        self.assertEqual(self.invoke('owner_account_export', cursor=cursor+'x')['statusCode'], 409)
        self.assertRaises(ValueError, a.read_cursor, cursor, OTHER, ['OWNER#'+OTHER])
        items = first['items']
        for _ in range(10):
            page = json.loads(self.invoke('owner_account_export', cursor=cursor)['body'])
            items.extend(page['items']); cursor = page['nextCursor']
            if page['complete']: break
        self.assertFalse(cursor); self.assertEqual(len(items), 31)
        encoded = json.dumps(items)
        self.assertNotIn('secret-', encoded); self.assertNotIn('+1202', encoded)
        self.assertIn('/api/v1/phone/recordings/', encoded)
        self.active_mock.return_value = False
        self.assertEqual(self.invoke('owner_account_export')['statusCode'],403)

    def test_purge_versions_provider_metadata_and_retry_are_owner_scoped(self):
        call = self.call()
        key = call['s3Key']
        r._s3.versions = [{'Key': key, 'VersionId': 'old'}, {'Key': key, 'VersionId': 'new'},
                          {'Key': 'phone-trial/other/'+CALL+'/private.mp3', 'VersionId': 'other'}]
        r._table.put_item(Item={**m.request_key(OWNER, 'request'), 'ownerUserId': OWNER, 'requestId': 'request', 'callbackId': 'c'*64})
        r._table.put_item(Item={'PK': 'OWNER_CALLBACK#'+'c'*64,'SK':'META','ownerUserId':OWNER,'requestId':'request'})
        r._table.put_item(Item={**m.request_key(OTHER,'other'),'ownerUserId':OTHER,'requestId':'other'})
        r._table.put_item(Item={**m.VOICE_KEY,'activeRequest':'owner-stop-2026-10-16'})
        for _ in range(12):
            result = self.invoke()
            self.assertEqual(result['statusCode'],200,result)
            if json.loads(result['body'])['complete']: break
        self.assertTrue(json.loads(result['body'])['complete'])
        self.assertEqual(self.deleted_provider,[(CALL,REC)])
        self.assertEqual(len(r._s3.deleted),2); self.assertEqual(len(r._s3.versions),1)
        self.assertIsNone(r.get(m.CONFIG_KEY)); self.assertIsNone(r.get({'PK':'CONFIG','SK':'INBOUND_OWNER'}))
        self.assertIsNotNone(r.get(m.request_key(OTHER,'other')))
        self.assertEqual(r.get(m.VOICE_KEY)['activeRequest'],'owner-stop-2026-10-16')
        self.assertTrue(r.get(a.owner_key(OWNER))['complete'])
        self.assertTrue(a.blocked(call))
        self.assertEqual(json.loads(self.invoke()['body']),{'complete':True})
        self.assertEqual(self.invoke('owner_account_export')['statusCode'],403)

    def test_copy_lease_and_object_errors_keep_metadata_for_retry(self):
        call = self.call(recordingStatus='copying',copyLeaseUntil=1120)
        response = self.invoke(); self.assertEqual(response['statusCode'],200) # empty owner-run advances
        response = self.invoke(); self.assertEqual(response['statusCode'],503)
        self.assertEqual(json.loads(response['body'])['retryAfterSeconds'],120)
        self.assertIsNotNone(r.get(r.call_key(CALL,'trial'))); self.assertFalse(self.deleted_provider)
        self.now.return_value=1121
        r._s3.versions=[{'Key':call['s3Key'],'VersionId':'one'}];r._s3.errors=True
        self.assertEqual(self.invoke()['statusCode'],503)
        self.assertIsNotNone(r.get(r.call_key(CALL,'trial')))
        r._s3.errors=False
        self.assertEqual(self.invoke()['statusCode'],200)
        self.assertIsNone(r.get(r.call_key(CALL,'trial')))

    def test_missing_provider_call_still_removes_owned_objects(self):
        call=self.call()
        with patch.object(m,'provider',side_effect=m.ProviderError(20404)):
            a.purge_call(call)
        self.assertEqual(self.deleted_provider,[(CALL,REC)])
        with patch.object(m,'provider',side_effect=m.ProviderError(20003)):
            self.assertRaises(m.ProviderError,a.purge_call,call)

    def test_foreign_provider_binding_or_object_listing_never_deletes(self):
        call=self.call()
        with patch.object(m,'provider',return_value={'sid':CALL,'account_sid':'other','status':'completed'}):
            self.assertRaises(ValueError,a.purge_call,call)
        self.assertFalse(self.deleted_provider)
        with patch.object(r._s3,'list_object_versions',return_value={'Versions':[{'Key':'other-owner','VersionId':'1'}]}):
            self.assertRaises(ValueError,a.delete_objects,f'phone-trial/trial/{CALL}/')
        self.assertFalse(r._s3.deleted)

    def test_tombstone_blocks_claim_ready_and_late_callback(self):
        call=self.call(recordingStatus='pending')
        r._table.put_item(Item={**a.run_key('trial'),'ownerUserId':OWNER})
        self.assertTrue(a.blocked(call))
        self.assertEqual(r.ingest_recording(CALL,REC,'completed')['statusCode'],200)
        self.assertEqual(r.get(r.call_key(CALL,'trial'))['recordingStatus'],'pending')
        for status in ('copying','ready'):
            with patch.object(r,'commit',REAL_COMMIT):
                self.assertRaises(Conflict,r.commit,call,{**call,'version':2,'recordingStatus':status})
        self.assertRaises(ValueError,a.cleanup_if_deleted,call,'other-owner/private.mp3')

    def test_only_authenticated_delete_can_establish_fence(self):
        self.active_mock.return_value=False
        self.assertEqual(self.invoke()['statusCode'],409)
        self.assertIsNone(r.get(a.owner_key(OWNER)))
        self.assertEqual(r.handler({'version':'2.0','operation':'owner_account_purge','ownerUserId':OWNER},None)['statusCode'],404)

    def test_revocation_check_uses_current_owner_profile(self):
        table = Mock()
        table.get_item.side_effect = [{'Item':{'userId':OWNER}}, {'Item':{'role':'owner','status':'active','tokensValidAfter':1000}}]
        boto = types.SimpleNamespace(resource=lambda *args,**kwargs:types.SimpleNamespace(Table=lambda _:table))
        with patch.dict(sys.modules,{'boto3':boto}):
            self.assertFalse(REAL_OWNER_ACTIVE(OWNER,since=1000))
        self.assertTrue(all(call.kwargs['ConsistentRead'] for call in table.get_item.call_args_list))

    def test_provider_delete_is_identity_bound_and_204_confirmed(self):
        result = Mock(status=204)
        result.__enter__=Mock(return_value=result);result.__exit__=Mock(return_value=False)
        opener = Mock();opener.open.return_value=result
        with patch.object(m,'provider',return_value={'sid':REC,'call_sid':CALL,'account_sid':ACCOUNT}), patch.object(a.urllib.request,'build_opener',return_value=opener):
            REAL_PROVIDER_DELETE(CALL,REC)
        request=opener.open.call_args.args[0]
        self.assertEqual(request.method,'DELETE');self.assertTrue(request.full_url.endswith('/Recordings/'+REC+'.json'))
        with patch.object(m,'provider',return_value={'sid':REC,'call_sid':'CA'+'9'*32,'account_sid':ACCOUNT}), patch.object(a.urllib.request,'build_opener') as network:
            self.assertRaises(ValueError,REAL_PROVIDER_DELETE,CALL,REC)
            network.assert_not_called()

    def test_release_only_exact_known_terminal_owner_call_lock(self):
        for active in ('owner:'+OWNER+':one','owner-stop-2026-10-16','owner:'+OTHER+':one'):
            with self.subTest(active=active):
                r._table.items={}
                r._table.put_item(Item=self.cfg)
                item={**m.request_key(OWNER,'one'),'ownerUserId':OWNER,'requestId':'one','lockId':'owner:'+OWNER+':one','callSid':CALL}
                r._table.put_item(Item=item)
                r._table.put_item(Item={**m.VOICE_KEY,'activeRequest':active})
                for _ in range(10):
                    response=self.invoke()
                    self.assertEqual(response['statusCode'],200,response)
                    if json.loads(response['body'])['complete']:break
                value=r.get(m.VOICE_KEY)
                if active==item['lockId']:self.assertNotIn('activeRequest',value)
                else:self.assertEqual(value['activeRequest'],active)

    def test_callback_deleted_before_message_failure_resumes(self):
        message = {**m.request_key(OWNER,'partial'),'ownerUserId':OWNER,'requestId':'partial','callbackId':'d'*64}
        callback = {'PK':'OWNER_CALLBACK#'+'d'*64,'SK':'META','ownerUserId':OWNER,'requestId':'partial'}
        r._table.put_item(Item=message);r._table.put_item(Item=callback)
        self.assertEqual(self.invoke()['statusCode'],200)
        self.assertEqual(self.invoke()['statusCode'],200)
        original=r._table.delete_item
        def fail_message(Key, **kwargs):
            if Key == m.request_key(OWNER,'partial'):
                raise RuntimeError('simulated crash after callback deletion')
            return original(Key,**kwargs)
        with patch.object(r._table,'delete_item',side_effect=fail_message):
            self.assertEqual(self.invoke()['statusCode'],503)
        self.assertIsNone(r.get({'PK':callback['PK'],'SK':'META'}))
        self.assertIsNotNone(r.get(m.request_key(OWNER,'partial')))
        self.assertEqual(self.invoke()['statusCode'],200)
        self.assertIsNone(r.get(m.request_key(OWNER,'partial')))
        self.assertTrue(json.loads(self.invoke()['body'])['complete'])


if __name__ == '__main__':
    unittest.main()
