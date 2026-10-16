import json
import unittest
from unittest.mock import Mock, patch
import owner_recordings as o
import owner_messages as m
import recorder as r
import test_recorder as fixtures
from test_recorder import CALL, REC

OWNER='11111111-1111-4111-8111-111111111111'
OTHER='22222222-2222-4222-8222-222222222222'

class RecordingAccessTests(unittest.TestCase):
    setUp=fixtures.RecorderTests.setUp
    tearDown=fixtures.RecorderTests.tearDown
    commit=fixtures.RecorderTests.commit

    def configure(self, **extra):
        r._table.put_item(Item={**m.CONFIG_KEY,'ownerUserId':OWNER,'enabled':False,**extra})

    def invoke(self, operation='owner_recordings_list', **extra):
        return r.handler({'operation':operation,'ownerUserId':OWNER,**extra},None)

    def ready(self, source='messages', owner=OWNER, **extra):
        run=m.namespace(OWNER) if source=='messages' else source
        call={**r.call_key(CALL,run),'ownerUserId':owner,'runId':run,'callSid':CALL,'recordingSid':REC,'recordingStatus':'ready','audioBytes':3,'s3Key':f'phone-trial/{run}/{CALL}/{REC}.mp3',**extra}
        r._table.put_item(Item=call)
        r._s3.data=b'MP3';r._s3.metadata={'sha256':'a'*64}
        return call

    def test_iam_and_owner_boundaries_and_explicit_legacy_binding(self):
        self.configure()
        self.assertEqual(self.invoke(ownerUserId=OTHER)['statusCode'],403)
        self.assertEqual(self.invoke(source='trial')['statusCode'],404)
        for extra in ({'version':'2.0'},{'requestContext':{}}):
            self.assertEqual(self.invoke(**extra)['statusCode'],404)
        self.configure(recordingRuns=['trial'])
        self.ready('trial')
        data=json.loads(self.invoke(source='trial')['body'])
        self.assertEqual(len(data['items']),1)
        self.assertNotIn('s3Key',data['items'][0])
        self.assertNotIn('recordingLink',data['items'][0])

    def test_list_queries_owner_partition_and_shows_unconnected_attempt(self):
        self.configure()
        message={**m.request_key(OWNER,'test'),'ownerUserId':OWNER,'requestId':'test','destination':'primary','providerStatus':'rejected','createdAt':1000}
        page={'Items':[message],'LastEvaluatedKey':m.request_key(OWNER,'test')}
        with patch.object(r._table,'query',return_value=page) as query:
            data=json.loads(self.invoke()['body'])
            self.assertEqual(data['items'][0]['recordingStatus'],'absent')
            self.assertEqual(data['nextCursor'],'test')
            self.assertEqual(query.call_args.kwargs['ExpressionAttributeValues'],{':pk':'OWNER#'+OWNER,':prefix':'MESSAGE#'})
            self.assertEqual(query.call_args.kwargs['Limit'],25)
            self.assertIn('begins_with',query.call_args.kwargs['KeyConditionExpression'])
        for value in (0,51,True,'25'):
            self.assertEqual(self.invoke(limit=value)['statusCode'],400)
        self.assertEqual(self.invoke(cursor='../other')['statusCode'],400)

    def test_ready_media_is_five_minutes_and_checks_binding(self):
        self.configure();call=self.ready()
        for disposition in ('inline','attachment'):
            with patch.object(r._s3,'generate_presigned_url',return_value='https://private.example/signed') as sign:
                self.assertEqual(self.invoke('owner_recording_media',callSid=CALL,disposition=disposition)['statusCode'],200)
                args=sign.call_args.kwargs
                self.assertEqual(args['ExpiresIn'],300)
                self.assertTrue(args['Params']['ResponseContentDisposition'].startswith(disposition+';'))
                self.assertEqual(args['Params']['ResponseCacheControl'],'private, no-store')
        self.ready(owner=OTHER)
        self.assertEqual(self.invoke('owner_recording_media',callSid=CALL,disposition='inline')['statusCode'],404)
        self.ready(s3Key='other-owner/private.mp3')
        self.assertEqual(self.invoke('owner_recording_media',callSid=CALL,disposition='inline')['statusCode'],409)
        self.ready(recordingStatus='failed')
        self.assertEqual(self.invoke('owner_recording_media',callSid=CALL,disposition='inline')['statusCode'],409)

    def test_missing_and_bad_integrity_never_sign(self):
        self.configure();self.ready()
        with patch.object(r._s3,'generate_presigned_url') as sign:
            r._s3.metadata={}
            self.assertEqual(self.invoke('owner_recording_media',callSid=CALL,disposition='inline')['statusCode'],409)
            class Missing(Exception):
                response={'Error':{'Code':'NoSuchKey'}}
            with patch.object(r._s3,'head_object',side_effect=Missing()):
                self.assertEqual(self.invoke('owner_recording_media',callSid=CALL,disposition='inline')['statusCode'],410)
            sign.assert_not_called()

    def test_deployment_binding_is_conditional_and_preserves_stop(self):
        main,phone,client=Mock(),Mock(),Mock()
        main.get_item.return_value={'Item':{'userId':OWNER}}
        phone.get_item.return_value={'Item':{'ownerUserId':OWNER,'enabled':False}}
        o.bind_legacy_recordings(main,phone,client)
        main.get_item.assert_called_once_with(Key={'pk':'CONFIG','sk':'OWNER'},ConsistentRead=True)
        ops=client.transact_write_items.call_args.kwargs['TransactItems']
        self.assertEqual(ops[1]['Update']['UpdateExpression'],'SET recordingRuns=:runs')
        self.assertIn('attribute_not_exists(recordingRuns)',ops[1]['Update']['ConditionExpression'])
        self.assertEqual(ops[1]['Update']['ExpressionAttributeValues'][':runs'],['hotel-trial-2026-10-15'])
        self.assertEqual(ops[0]['ConditionCheck']['ExpressionAttributeValues'][':owner'],OWNER)
        client.reset_mock();phone.get_item.return_value={'Item':{'ownerUserId':OTHER}}
        with self.assertRaises(ValueError):o.bind_legacy_recordings(main,phone,client)
        client.transact_write_items.assert_not_called()

if __name__=='__main__':unittest.main()
