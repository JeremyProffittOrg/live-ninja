import copy
import hashlib
import hmac
import base64
import io
import json
import os
import unittest
import urllib.parse
from email.message import Message
from unittest.mock import patch

import recorder as r
import send_report
from email.message import EmailMessage
from decimal import Decimal

CALL = "CA" + "1" * 32
REC = "RE" + "2" * 32
ACCOUNT = "AC" + "3" * 32


class Conflict(Exception):
    response = {"Error": {"Code": "ConditionalCheckFailedException"}}


class Table:
    def __init__(self):
        self.items = {}

    def get_item(self, Key, **kwargs):
        value = self.items.get((Key["PK"], Key["SK"]))
        return {"Item": copy.deepcopy(value)} if value else {}

    def put_item(self, Item, ConditionExpression=None):
        key = (Item["PK"], Item["SK"])
        if ConditionExpression and key in self.items:
            raise Conflict()
        self.items[key] = copy.deepcopy(Item)

    def query(self, **kwargs):
        pk = kwargs["ExpressionAttributeValues"][":pk"]
        return {"Items": [copy.deepcopy(v) for (p, _), v in self.items.items() if p == pk]}


class S3:
    def __init__(self):
        self.data = b""
        self.ttl = None
        self.metadata = {}
        self.content_type = None

    def put_object(self, **kwargs):
        assert kwargs["ServerSideEncryption"] == "AES256"
        self.data = kwargs["Body"].read()
        assert kwargs["ChecksumSHA256"] == base64.b64encode(hashlib.sha256(self.data).digest()).decode()
        self.metadata = kwargs["Metadata"]
        self.content_type = kwargs["ContentType"]

    def head_object(self, **kwargs):
        return {"ContentLength": len(self.data), "ContentType": self.content_type, "Metadata": self.metadata}

    def generate_presigned_url(self, operation, **kwargs):
        self.ttl = kwargs["ExpiresIn"]
        return "https://private.example/signed"


class Audio(io.BytesIO):
    def __init__(self, value=b"MP3", content_type="audio/mpeg"):
        super().__init__(value)
        self.headers = Message()
        self.headers["Content-Type"] = content_type


class RecorderTests(unittest.TestCase):
    def setUp(self):
        self.env = patch.dict(os.environ, {"TABLE": "test", "BUCKET": "private", "RUN_ID": "trial", "PUBLIC_BASE_URL": "https://api.example", "TWILIO_ACCOUNT_SID": ACCOUNT, "TWILIO_AUTH_TOKEN": "test-secret"})
        self.env.start()
        r._table, r._s3 = Table(), S3()
        self.commit_patch = patch.object(r, "commit", self.commit)
        self.commit_patch.start()
        self.clock = patch.object(r.time, "time", return_value=1000)
        self.now = self.clock.start()
        # Provider deletion never reaches the network in tests.
        self.provider_delete = patch.object(r, "delete_provider_audio", return_value="deleted")
        self.deleted = self.provider_delete.start()

    def tearDown(self):
        self.provider_delete.stop()
        self.env.stop()
        self.commit_patch.stop()
        self.clock.stop()
        r._table = r._s3 = None

    def commit(self, old, new, event=None):
        current = r.get({k: new[k] for k in ("PK", "SK")})
        if current != old:
            raise Conflict()
        if event and r.get({k: event[k] for k in ("PK", "SK")}):
            raise Conflict()
        r._table.put_item(Item=new)
        if event:
            r._table.put_item(Item=event)

    def post(self, path, data, signed=True):
        raw = json.dumps(data)
        return r.handler({"rawPath": path, "requestContext": {"http": {"method": "POST"}}, "body": raw, "headers": {"X-LN-Signature": r.digest(raw.encode()) if signed else "bad"}}, None)

    def register(self):
        return self.post("/events", {"runId": "trial", "callSid": CALL, "type": "call_registered", "targetId": "hotel"})

    def callback(self, **overrides):
        data = {"AccountSid": ACCOUNT, "CallSid": CALL, "RecordingSid": REC, "RecordingStatus": "completed", **overrides}
        signed = "https://api.example/recording" + "".join(k + data[k] for k in sorted(data))
        signature = base64.b64encode(hmac.new(b"test-secret", signed.encode(), hashlib.sha1).digest()).decode()
        return r.handler({"rawPath": "/recording", "requestContext": {"http": {"method": "POST"}}, "body": urllib.parse.urlencode(data), "headers": {"X-Twilio-Signature": signature}}, None)

    def ready(self):
        self.register()
        r.set_recording(CALL, REC, "ready", s3Key="phone-trial/trial/" + CALL + "/" + REC + ".mp3")

    def test_budget_dispatch_is_iam_only(self):
        with patch("owner_messages.budget_operation", return_value={"statusCode": 200}) as dispatch:
            for operation in ("owner_budget_status", "owner_budget_grant"):
                event = {"operation": operation}
                self.assertEqual(r.handler(event, None)["statusCode"], 200)
                dispatch.assert_called_with(event)
                dispatch.reset_mock()
                self.assertEqual(self.post("/budget", event)["statusCode"], 404)
                dispatch.assert_not_called()

    def test_signatures_rejected_before_parsing(self):
        self.assertEqual(self.post("/events", {}, False)["statusCode"], 403)
        result = r.handler({"rawPath": "/recording", "requestContext": {"http": {"method": "POST"}}, "body": "", "headers": {}}, None)
        self.assertEqual(result["statusCode"], 403)
        self.assertEqual(r._table.items, {})

    def test_events_idempotent_and_target_immutable(self):
        self.assertEqual(self.register()["statusCode"], 200)
        self.assertTrue(json.loads(self.register()["body"])["duplicate"])
        self.assertEqual(len(r._table.items), 2)
        result = self.post("/events", {"runId": "trial", "callSid": CALL, "type": "summary", "targetId": "other"})
        self.assertEqual(result["statusCode"], 409)

    def test_late_trial_registration_cannot_recreate_deleted_run(self):
        r._table.put_item(Item={'PK': 'RUN#trial', 'SK': 'ACCOUNT_DELETION', 'complete': True})
        result = self.register()
        self.assertEqual(result['statusCode'], 410)
        self.assertEqual(json.loads(result['body'])['recordingStatus'], 'deleted')
        self.assertIsNone(r.get(r.call_key(CALL)))
        self.assertEqual(len(r._table.items), 1)

    def test_recording_intent_survives_failure_and_late_deleted_callback_uses_cleanup(self):
        self.register()
        self.state_event('recording_requested', outcome='attempted')
        self.state_event('error', code='recording_start_failed')
        self.assertEqual(r.get(r.call_key(CALL))['recordingCreateState'], 'attempted')
        r._table.put_item(Item={'PK': 'RUN#trial', 'SK': 'ACCOUNT_DELETION', 'ownerUserId': 'owner'})
        with patch('account_lifecycle.resolve_deleted_recording', return_value=r.response(200)) as resolve, patch.object(r, 'copy_audio') as copy_audio:
            self.assertEqual(self.callback(AccountSid='AC'+'9'*32)['statusCode'], 400)
            resolve.assert_not_called()
            self.assertEqual(self.callback()['statusCode'], 200)
            self.assertEqual(resolve.call_args.args[1:], (REC, 'completed'))
            self.assertEqual(resolve.call_args.args[0]['callSid'], CALL)
            copy_audio.assert_not_called()

    def test_wrong_account_or_unregistered_call_rejected(self):
        self.assertEqual(self.callback()["statusCode"], 409)
        self.register()
        self.assertEqual(self.callback(AccountSid="AC" + "4" * 32)["statusCode"], 400)

    def test_canonical_audio_url_ignores_callback_url_and_becomes_ready(self):
        self.register()
        class Opener:
            def open(inner, request, timeout):
                self.assertEqual(request.full_url, f"https://api.twilio.com/2010-04-01/Accounts/{ACCOUNT}/Recordings/{REC}.mp3")
                self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], "copying")
                return Audio()
        with patch.object(r.urllib.request, "build_opener", return_value=Opener()):
            self.assertEqual(self.callback(RecordingUrl="http://169.254.169.254/")["statusCode"], 200)
        self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], "ready")
        self.assertEqual(r._s3.data, b"MP3")
        with patch.object(r, "copy_audio", side_effect=AssertionError("duplicate fetched")):
            self.assertEqual(self.callback()["statusCode"], 200)
        self.assertEqual(self.callback(RecordingSid="RE" + "5" * 32)["statusCode"], 409)

    def test_verified_copy_deletes_provider_copy_once(self):
        self.register()
        key = "phone-trial/trial/" + CALL + "/" + REC + ".mp3"
        def deleting(sid, recording):
            row = r.get(r.call_key(CALL))
            # Never before the S3 object is stored and verified.
            self.assertEqual((row["recordingStatus"], row["s3Key"], r._s3.data), ("ready", key, b"MP3"))
            return "deleted"
        self.deleted.side_effect = deleting
        opener = unittest.mock.Mock()
        opener.open.return_value = Audio()
        with patch.object(r.urllib.request, "build_opener", return_value=opener):
            self.assertEqual(self.callback()["statusCode"], 200)
        self.deleted.assert_called_once_with(CALL, REC)
        call = r.get(r.call_key(CALL))
        self.assertEqual((call["providerCopyState"], call["providerCopyDeletedAt"], call["sourceRetained"], call["providerCopyDeleteAttempts"]),
                         ("deleted", 1000, False, 1))
        self.assertEqual(self.callback()["statusCode"], 200)
        self.deleted.assert_called_once()

    def test_failed_s3_verification_keeps_provider_copy_and_fails_visibly(self):
        self.register()
        opener = unittest.mock.Mock()
        opener.open.side_effect = lambda *args, **kwargs: Audio()
        with patch.object(r.urllib.request, "build_opener", return_value=opener),                 patch.object(r._s3, "head_object", return_value={"ContentLength": 3, "ContentType": "audio/mpeg", "Metadata": {"sha256": "mismatch"}}):
            self.assertEqual(self.callback()["statusCode"], 503)
        call = r.get(r.call_key(CALL))
        self.assertEqual((call["recordingStatus"], call["providerCopyState"], call["sourceRetained"]), ("failed", "retained", True))
        self.assertEqual(call["recordingReason"], "copy_failed")
        self.deleted.assert_not_called()
        stored = {**call, "recordingStatus": "ready", "audioBytes": 3, "s3Key": "phone-trial/trial/" + CALL + "/" + REC + ".mp3"}
        for content_type in ("binary/octet-stream", None):
            with self.subTest(content_type=content_type), patch.object(r._s3, "head_object", return_value={
                    "ContentLength": 3, "ContentType": content_type, "Metadata": {"sha256": "a" * 64}}):
                self.assertFalse(r.verified_s3_copy(stored))

    def test_reverification_failure_before_delete_keeps_provider_copy(self):
        self.ready()
        call = r.get(r.call_key(CALL))
        call.update(audioBytes=3, providerCopyState="pending")
        r._table.put_item(Item=call)
        # The stored object is gone or altered: the provider copy is the only one left.
        self.assertEqual(r.release_provider_copy(CALL, REC), "retained")
        call = r.get(r.call_key(CALL))
        self.assertEqual((call["providerCopyState"], call["providerCopyReason"]), ("retained", "s3_verification_failed"))
        self.assertNotIn("providerCopyDeletedAt", call)
        self.deleted.assert_not_called()
        self.assertEqual(self.callback()["statusCode"], 200)
        self.deleted.assert_not_called()

    def test_provider_copy_delete_retries_are_bounded_to_three(self):
        self.register()
        self.deleted.side_effect = RuntimeError("Provider recording deletion failed")
        opener = unittest.mock.Mock()
        opener.open.return_value = Audio()
        with patch.object(r.urllib.request, "build_opener", return_value=opener):
            self.assertEqual(self.callback()["statusCode"], 503)
        self.assertEqual(r.get(r.call_key(CALL))["providerCopyState"], "pending")
        self.assertEqual(self.callback()["statusCode"], 503)
        self.assertEqual(self.deleted.call_count, 1)
        self.now.return_value = 1030
        self.assertEqual(self.callback()["statusCode"], 503)
        self.now.return_value = 1090
        self.assertEqual(self.callback()["statusCode"], 200)
        self.now.return_value = 5000
        self.assertEqual(self.callback()["statusCode"], 200)
        self.assertEqual(self.deleted.call_count, 3)
        call = r.get(r.call_key(CALL))
        self.assertEqual((call["recordingStatus"], call["providerCopyState"], call["providerCopyReason"], call["sourceRetained"]),
                         ("ready", "retained", "delete_attempt_limit", True))
        self.assertEqual(call["providerCopyDeleteAttempts"], 3)

    def test_provider_copy_delete_treats_404_as_already_deleted(self):
        import account_lifecycle as a
        import owner_messages as m
        self.provider_delete.stop()
        try:
            with patch.object(m, "provider", side_effect=m.ProviderError(20404, True)),                     patch.object(a.urllib.request, "build_opener") as network:
                self.assertEqual(r.delete_provider_audio(CALL, REC), "deleted")
                network.assert_not_called()
            gone = a.urllib.error.HTTPError("https://api.twilio.com", 404, "Not Found", None, None)
            opener = unittest.mock.Mock()
            opener.open.side_effect = gone
            with patch.object(m, "provider", return_value={"sid": REC, "call_sid": CALL, "account_sid": ACCOUNT, "status": "completed"}),                     patch.object(a.urllib.request, "build_opener", return_value=opener):
                self.assertEqual(r.delete_provider_audio(CALL, REC), "deleted")
            self.assertEqual(opener.open.call_args.args[0].method, "DELETE")
        finally:
            self.provider_delete.start()

    def test_missing_audio_and_failed_copy_are_visible(self):
        self.register()
        self.assertEqual(self.callback(RecordingStatus="absent")["statusCode"], 200)
        self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], "absent")
        with patch.object(r, "copy_audio", side_effect=OSError("provider unavailable")):
            self.assertEqual(self.callback()["statusCode"], 503)
        self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], "failed")

    def test_copy_retry_budget_and_backoff_survive_callbacks(self):
        self.register()
        with patch.object(r, "copy_audio", side_effect=OSError("unavailable")) as fetch:
            self.assertEqual(self.callback()["statusCode"], 503)
            self.assertEqual(self.callback()["statusCode"], 503)
            self.assertEqual(fetch.call_count, 1)
            self.now.return_value = 1030
            self.assertEqual(self.callback()["statusCode"], 503)
            self.now.return_value = 1089
            self.assertEqual(self.callback()["statusCode"], 503)
            self.assertEqual(fetch.call_count, 2)
            self.now.return_value = 1090
            self.assertEqual(self.callback()["statusCode"], 200)
            self.now.return_value = 2000
            self.assertEqual(self.callback()["statusCode"], 200)
            self.assertEqual(fetch.call_count, 3)
        call = r.get(r.call_key(CALL))
        self.assertEqual(call["copyAttempts"], 3)
        self.assertEqual(call["recordingStatus"], "failed")
        self.assertEqual(call["recordingReason"], "copy_attempt_limit")
        self.assertEqual(self.callback(RecordingStatus="absent")["statusCode"], 200)
        self.assertEqual(r.get(r.call_key(CALL)), call)

    def test_overlapping_callbacks_cannot_copy_or_downgrade_ready(self):
        self.register()
        def copying(*args, **kwargs):
            self.assertEqual(self.callback()["statusCode"], 503)
            self.assertEqual(self.callback(RecordingStatus="absent")["statusCode"], 503)
            return "key", 3
        with patch.object(r, "copy_audio", side_effect=copying) as fetch:
            self.assertEqual(self.callback()["statusCode"], 200)
            fetch.assert_called_once()
        self.assertEqual(self.callback(RecordingStatus="absent")["statusCode"], 200)
        self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], "ready")

    def test_abandoned_copy_lease_counts_attempt_and_rejects_stale_completion(self):
        self.register()
        call = r.get(r.call_key(CALL))
        call.update(recordingSid=REC, recordingStatus="copying", copyAttempts=1, copyLeaseUntil=1120)
        r._table.put_item(Item=call)
        with patch.object(r, "copy_audio", return_value=("key", 3)) as fetch:
            self.now.return_value = 1119
            self.assertEqual(self.callback()["statusCode"], 503)
            fetch.assert_not_called()
            self.now.return_value = 1120
            self.assertEqual(self.callback()["statusCode"], 200)
        self.assertEqual(r.get(r.call_key(CALL))["copyAttempts"], 2)
        r.set_recording(CALL, REC, "failed", expected_attempt=1)
        self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], "ready")

    def test_deleted_and_expired_recordings_stay_unavailable(self):
        self.register()
        for state in ("deleted", "expired"):
            call = r.get(r.call_key(CALL))
            call.update(recordingStatus=state, recordingSid=REC)
            r._table.put_item(Item=call)
            with patch.object(r, "copy_audio") as fetch:
                self.assertEqual(self.callback()["statusCode"], 200)
                self.assertEqual(self.callback(RecordingStatus="absent")["statusCode"], 200)
                fetch.assert_not_called()
            r.set_recording(CALL, REC, "ready", expected_attempt=1)
            self.state_event("consent", result="declined")
            self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], state)

    def test_expired_final_lease_fails_without_fourth_transfer(self):
        self.register()
        call = r.get(r.call_key(CALL))
        call.update(recordingSid=REC, recordingStatus="copying", copyAttempts=3, copyLeaseUntil=999)
        r._table.put_item(Item=call)
        with patch.object(r, "copy_audio") as fetch:
            self.assertEqual(self.callback()["statusCode"], 200)
            fetch.assert_not_called()
        self.assertEqual(r.get(r.call_key(CALL))["recordingReason"], "copy_attempt_limit")

    def test_stale_worker_cannot_complete_new_attempt(self):
        self.register()
        call = r.get(r.call_key(CALL))
        call.update(recordingSid=REC, recordingStatus="copying", copyAttempts=2, copyLeaseUntil=1120)
        r._table.put_item(Item=call)
        for status in ("ready", "failed"):
            r.set_recording(CALL, REC, status, expected_attempt=1)
            self.assertEqual(r.get(r.call_key(CALL)), call)

    def test_claim_conflict_reloads_active_worker_without_copying(self):
        self.register()
        def competing_claim(old, new, event=None):
            self.commit(old, new, event)
            raise Conflict()
        with patch.object(r, "commit", side_effect=competing_claim), patch.object(r, "copy_audio") as fetch:
            self.assertEqual(self.callback()["statusCode"], 503)
            fetch.assert_not_called()
        self.assertEqual(r.get(r.call_key(CALL))["copyAttempts"], 1)

    def test_audio_empty_wrong_type_and_size_are_rejected(self):
        for content, kind, maximum in [(b"", "audio/mpeg", 10), (b"html", "text/html", 10), (b"abc", "audio/mpeg", 2)]:
            with self.subTest(kind=kind, maximum=maximum):
                opener = unittest.mock.Mock()
                opener.open.return_value = Audio(content, kind)
                with patch.object(r.urllib.request, "build_opener", return_value=opener), patch.object(r, "MAX_AUDIO", maximum):
                    with self.assertRaises(ValueError):
                        r.copy_audio(CALL, REC)
        self.assertEqual(r._s3.data, b"")

    def test_report_tokens_do_not_extend_and_download_expires(self):
        self.ready()
        report = json.loads(self.post("/report", {"runId": "trial"})["body"])
        link = report["calls"][0]["recordingLink"]
        token = urllib.parse.parse_qs(urllib.parse.urlsplit(link["url"]).query)["token"][0]
        self.assertFalse(any(token in json.dumps(i) for i in r._table.items.values()))
        self.assertEqual(r.download({"token": token})["statusCode"], 302)
        self.assertEqual(r._s3.ttl, 900)
        with patch.object(r.time, "time", return_value=link["expiresAt"] - 60):
            self.assertEqual(r.issue_link(r.get(r.call_key(CALL))), link)
            self.assertEqual(r.download({"token": token})["statusCode"], 302)
            self.assertEqual(r._s3.ttl, 60)
        with patch.object(r.time, "time", return_value=link["expiresAt"]):
            self.assertEqual(r.download({"token": token})["statusCode"], 404)
        call = r.get(r.call_key(CALL))
        call["s3Key"] = "different"
        r._table.put_item(Item=call)
        self.assertEqual(r.download({"token": token})["statusCode"], 404)

    def test_internal_report_requires_direct_iam_invocation(self):
        self.register()
        self.assertEqual(r.handler({"operation": "internal_report", "runId": "trial"}, None)["statusCode"], 200)
        for extra in ({"version": "2.0"}, {"requestContext": {"http": {"method": "POST"}}}):
            self.assertEqual(r.handler({"operation": "internal_report", "runId": "trial", **extra}, None)["statusCode"], 404)
        self.assertEqual(self.post("/report", {"operation": "internal_report", "runId": "trial"}, signed=False)["statusCode"], 403)

    def test_email_distinguishes_registration_from_completed_call(self):
        report = {"runId": "trial", "calls": [{"callSid": CALL, "targetId": "<Hotel>", "recordingStatus": "pending"}], "events": []}
        outcome, body = send_report.build_report(report, "{{OUTCOME_ONE_LINE}} {{ACTUAL_COMMAND_OUTPUT}}")
        self.assertIn("0 provider-completed calls; 0 S3 recordings ready", outcome)
        self.assertIn("&lt;Hotel&gt;", body)
        self.assertIn("No verified S3 recording", body)

    def test_reconcile_verifies_provider_call_binding(self):
        self.register()
        event = {"operation": "internal_reconcile", "runId": "trial", "callSid": CALL, "recordingSid": REC}
        opener = unittest.mock.Mock()
        for binding, expected in [(CALL, 200), ("CA" + "9" * 32, 409)]:
            metadata = {"account_sid": ACCOUNT, "call_sid": binding, "sid": REC, "status": "completed"}
            opener.open.return_value = Audio(json.dumps(metadata).encode(), "application/json")
            with patch.object(r.urllib.request, "build_opener", return_value=opener), patch.object(r, "copy_audio", return_value=("key", 3)):
                self.assertEqual(r.handler(event, None)["statusCode"], expected)
        self.assertEqual(r.handler({**event, "version": "2.0"}, None)["statusCode"], 404)

    def state_event(self, kind, **details):
        return self.post("/events", {"runId": "trial", "callSid": CALL, "type": kind, "data": details})

    def test_consent_status_and_recording_absence(self):
        self.register()
        self.assertEqual(self.state_event("consent", result="granted")["statusCode"], 200)
        self.assertEqual(r.get(r.call_key(CALL))["consentStatus"], "granted")
        self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], "pending")
        self.state_event("consent", result="declined")
        call = r.get(r.call_key(CALL))
        self.assertEqual(call["recordingStatus"], "absent")
        self.assertEqual(call["recordingReason"], "consent_declined")

    def test_recording_start_error_and_unanswered_call_states(self):
        self.register()
        self.state_event("error", code="recording_start_failed")
        self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], "failed")
        for status in ("no-answer", "busy", "canceled", "failed"):
            r._table = Table()
            self.register()
            self.state_event("call_ended", status=status)
            call = r.get(r.call_key(CALL))
            self.assertEqual(call["recordingStatus"], "absent")
            self.assertEqual(call["recordingReason"], "call_" + status)

    def test_navigation_exit_has_no_recording_or_false_refusal(self):
        for reason in ("navigation_limit", "unsupported_menu", "voicemail"):
            r._table = Table()
            self.register()
            self.state_event("call_ended", reason=reason)
            call = r.get(r.call_key(CALL))
            self.assertEqual(call["recordingStatus"], "absent")
            self.assertEqual(call["recordingReason"], reason)
            self.assertNotIn("consentStatus", call)

    def test_navigation_summary_is_not_a_wifi_finding(self):
        import send_report
        report = {"calls": [{"callSid": CALL}], "events": [{"SK": "navigation", "data": {"callSid": CALL, "type": "summary", "data": {"phase": "navigation", "action": "digits", "digit": "2"}}}]}
        body = send_report.build_report(report, "")[1]
        self.assertIn("requirements remain unknown", body)
        self.assertNotIn("AI summary of staff-reported information", body)
        report["events"][0]["data"]["data"] = {"phase": "media", "action": "stream_authenticated"}
        self.assertIn("requirements remain unknown", send_report.build_report(report, "")[1])

    def test_ready_audio_preserved_and_final_usage_stored(self):
        self.ready()
        self.state_event("consent", result="declined")
        self.state_event("error", code="recording_start_failed")
        details = {"status": "failed", "usageComplete": True, "inputTokens": 123, "outputTokens": 45}
        self.state_event("call_ended", **details)
        self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], "ready")
        report = json.loads(self.post("/report", {"runId": "trial"})["body"])
        ended = [e["data"]["data"] for e in report["events"] if e["data"]["type"] == "call_ended"]
        self.assertEqual(ended, [details])

    def test_cost_only_report_does_not_issue_recording_links(self):
        self.ready()
        event = {"operation": "internal_report", "runId": "trial", "includeLinks": False}
        report = json.loads(r.handler(event, None)["body"])
        self.assertNotIn("recordingLink", report["calls"][0])
        self.assertFalse(any(pk.startswith(("LINK#", "TOKEN#")) for pk, _ in r._table.items))
        with patch.object(r.time, "time", return_value=2000):
            linked = json.loads(r.handler({"operation": "internal_report", "runId": "trial"}, None)["body"])
        link = linked["calls"][0]["recordingLink"]
        self.assertEqual(link["expiresAt"], 2000 + r.LINK_SECONDS)
        with patch.object(r.time, "time", return_value=3000):
            r.handler(event, None)
            linked_again = json.loads(r.handler({"operation": "internal_report", "runId": "trial"}, None)["body"])
        self.assertEqual(linked_again["calls"][0]["recordingLink"], link)

    def test_control_and_blocked_email_report(self):
        control = {"PK": "TRIAL#trial", "SK": "CONTROL", "status": "blocked", "attempted": 0, "authorizedCalls": 20,
                   "detail": "Monthly allowance requires permission", "spentUSD": 19, "reservedInfrastructureUSD": 8}
        r._table.put_item(Item=control)
        report = json.loads(r.handler({"operation": "internal_report", "runId": "trial", "includeLinks": False}, None)["body"])
        self.assertEqual(report["control"], control)
        outcome, body = send_report.build_report(report, "{{OUTCOME_ONE_LINE}} {{ACTUAL_COMMAND_OUTPUT}}", [{"id": "hotel", "name": "Hotel Example"}])
        self.assertIn("Blocked: 0/20 attempted", outcome)
        self.assertIn("Hotel Example", body)
        self.assertIn("not a measured invoice", body)
        self.assertIn("Monthly allowance requires permission", body)
        self.assertNotIn("/download?token=", body)

    def test_email_completed_count_uses_provider_status(self):
        report = {"calls": [{"callSid": CALL}], "events": [{"data": {"callSid": CALL, "type": "call_ended", "data": {"reason": "disconnect"}}}]}
        self.assertIn("0 provider-completed calls", send_report.build_report(report, "")[0])
        report["events"].append({"data": {"callSid": CALL, "type": "call_ended", "data": {"status": "completed"}}})
        self.assertIn("1 provider-completed calls", send_report.build_report(report, "")[0])

    def test_email_compact_results_links_and_escaping(self):
        call = {"callSid": CALL, "targetId": "hotel", "consentStatus": "granted", "recordingStatus": "ready", "recordingLink": {"url": "https://api.example/download?token=actual", "expiresAt": 700000}}
        summary = {"summary": "Password & accept <button>", "evidence": "staff_report_only"}
        report = {"calls": [call], "events": [{"data": {"callSid": CALL, "type": "summary", "data": summary}}], "control": {"gitSha": "abc123"}}
        _, body = send_report.build_report(report, "{{SHA}} {{HEAD_SHA}} {{ITEM}}", [{"id": "hotel", "name": "Hotel <One>", "city": "Los Angeles"}])
        self.assertIn("Hotel &lt;One&gt; — Los Angeles", body)
        self.assertIn("Password &amp; accept &lt;button&gt;", body)
        self.assertLess(body.index("Listen to or download"), body.index("AI summary"))
        self.assertIn("abc123 abc123", body)
        self.assertIn("not VPN-tested", body)
        self.assertIn("full interaction logs remain in DynamoDB", body)
        self.assertNotIn('"events":', body)
        call.update(recordingStatus="absent", consentStatus="declined", recordingReason="consent_declined")
        report["events"] = []
        _, body = send_report.build_report(report, "")
        self.assertNotIn("download?token=", body)
        self.assertIn("consent_declined", body)
        self.assertIn("requirements remain unknown", body)

    def test_email_transcript_excerpt_is_staff_only(self):
        report = {"calls": [{"callSid": CALL}], "events": [
            {"data": {"callSid": CALL, "type": "transcript", "data": {"speaker": "hotel", "text": "Accept <terms>"}}},
            {"data": {"callSid": CALL, "type": "transcript", "data": {"speaker": "assistant", "text": "Unverified assistant claim"}}}]}
        _, body = send_report.build_report(report, "")
        self.assertIn("Staff transcript excerpt", body)
        self.assertIn("Accept &lt;terms&gt;", body)
        self.assertNotIn("Unverified assistant claim", body)

    def test_email_send_once_and_unknown_delivery(self):
        control = json.loads('{"status":"completed","attempted":20.0,"authorizedCalls":20.0}')
        report = {"runId": "trial", "control": control}
        class MailTable:
            def __init__(self):
                self.item = dict(control)
            def get_item(self, **kwargs):
                return {"Item": dict(self.item)}
            def update_item(self, **kwargs):
                values = kwargs["ExpressionAttributeValues"]
                assert not any(isinstance(v, float) for v in values.values())
                if ":id" in values:
                    self.item.update(mailState="sent", sesMessageId=values[":id"])
                else:
                    self.item.update(mailState="sending", mailOutcome=values[":outcome"])
        table = MailTable()
        ses = unittest.mock.Mock()
        ses.send_raw_email.return_value = {"MessageId": "test-id"}
        message = EmailMessage()
        message.set_content("Test", subtype="html")
        self.assertEqual(send_report.send_once(table, ses, report, message), ("test-id", True))
        self.assertEqual(table.item["mailOutcome"], "completed:20")
        control["attempted"] = 20
        self.assertEqual(send_report.send_once(table, ses, report, message), ("test-id", False))
        self.assertEqual(ses.send_raw_email.call_count, 1)
        control["transcriptionStatus"] = "failed"
        self.assertEqual(send_report.send_once(table, ses, report, message), ("test-id", True))
        self.assertEqual(table.item["mailOutcome"], "completed:20:transcription:failed")
        control["transcriptionStatus"] = "completed"
        self.assertEqual(send_report.send_once(table, ses, report, message), ("test-id", True))
        self.assertEqual(send_report.send_once(table, ses, report, message), ("test-id", False))
        self.assertEqual(ses.send_raw_email.call_count, 3)
        table = MailTable()
        ses.send_raw_email.side_effect = TimeoutError()
        with self.assertRaisesRegex(RuntimeError, "outcome is unknown"):
            send_report.send_once(table, ses, report, message)
        self.assertEqual(table.item["mailState"], "sending")
        with self.assertRaisesRegex(RuntimeError, "outcome is unknown"):
            send_report.send_once(table, ses, report, message)
        self.assertEqual(ses.send_raw_email.call_count, 4)

    def test_email_count_boundary_rejects_invalid_counts(self):
        outcome, _ = send_report.build_report({"control": {"status": "blocked", "attempted": 0.0, "authorizedCalls": 20.0}}, "")
        self.assertIn("0/20 attempted", outcome)
        for value in (0.5, -1, float("nan"), float("inf"), True, "0", None):
            with self.subTest(value=value):
                table, ses = unittest.mock.Mock(), unittest.mock.Mock()
                with self.assertRaisesRegex(ValueError, "nonnegative integer"):
                    send_report.send_once(table, ses, {"runId": "trial", "control": {"status": "blocked", "attempted": value}}, EmailMessage())
                table.get_item.assert_not_called()
                table.update_item.assert_not_called()
                ses.send_raw_email.assert_not_called()

    def test_twenty_call_email_omits_large_transcript_logs(self):
        calls, events = [], []
        for number in range(20):
            sid = "CA" + f"{number:032x}"
            calls.append({"callSid": sid, "targetId": "hotel-" + str(number), "recordingStatus": "absent"})
            for _ in range(10):
                events.append({"data": {"callSid": sid, "type": "transcript", "data": {"speaker": "hotel", "text": "A" * 2000}}})
        _, body = send_report.build_report({"calls": calls, "events": events}, "")
        self.assertLess(len(body.encode()), 60000)
        self.assertEqual(body.count("Staff transcript excerpt"), 20)

    def test_audio_declared_length_and_checksum_integrity(self):
        for length in ("10", "-1", "not-a-number", "0", "2"):
            source = Audio(b"MP3")
            source.headers["Content-Length"] = length
            opener = unittest.mock.Mock()
            opener.open.return_value = source
            with self.subTest(length=length), patch.object(r.urllib.request, "build_opener", return_value=opener):
                with self.assertRaises(ValueError):
                    r.copy_audio(CALL, REC)
            self.assertEqual(r._s3.data, b"")
        source = Audio(b"MP3")
        source.headers["Content-Length"] = "3"
        opener.open.return_value = source
        with patch.object(r.urllib.request, "build_opener", return_value=opener):
            r.copy_audio(CALL, REC)
        self.assertEqual(r._s3.metadata["sha256"], hashlib.sha256(b"MP3").hexdigest())
        opener.open.return_value = Audio(b"MP3")
        with patch.object(r.urllib.request, "build_opener", return_value=opener), patch.object(r._s3, "head_object", return_value={"ContentLength": 3, "Metadata": {"sha256": "mismatch"}}):
            with self.assertRaisesRegex(ValueError, "S3 verification failed"):
                r.copy_audio(CALL, REC)

    def test_internal_report_filters_before_recording_link_issuance(self):
        self.ready()
        other_sid = "CA" + "9" * 32
        r._table.put_item(Item={**r.call_key(other_sid), "runId": "trial", "callSid": other_sid,
            "recordingStatus": "ready", "recordingSid": "RE" + "9" * 32, "s3Key": "unrelated"})
        result = r.handler({"operation": "internal_report", "runId": "trial", "callSids": [CALL]}, None)
        report = json.loads(result["body"])
        self.assertEqual([call["callSid"] for call in report["calls"]], [CALL])
        self.assertTrue(all(row["data"]["callSid"] == CALL for row in report["events"]))
        self.assertEqual(len([key for key in r._table.items if key[0].startswith("LINK#")]), 1)
        self.assertIsNone(r.get({"PK": "LINK#RE" + "9" * 32, "SK": "META"}))
        empty = json.loads(r.handler({"operation": "internal_report", "runId": "trial", "callSids": []}, None)["body"])
        self.assertEqual(empty["calls"], [])
        self.assertEqual(empty["events"], [])
        for invalid in ([CALL] * 21, [CALL, CALL], ["invalid"], "invalid", [123]):
            self.assertEqual(r.handler({"operation": "internal_report", "runId": "trial", "callSids": invalid}, None)["statusCode"], 400)

    def test_batch_context_uses_only_approved_targets_and_call_sids(self):
        batch = "hotel-vpn-ten-2026-10-16"
        control = {"PK": "TRIAL#" + batch, "SK": "CONTROL", "status": "completed", "eventRunId": "hotel-trial-2026-10-15",
                   "attempted": Decimal("1"), "authorizedCalls": Decimal("10"), "spentUSD": Decimal("17.58806")}
        r._table.put_item(Item=control)
        targets = [{"id": "hotel-%02d" % number, "name": "Hotel " + str(number)} for number in range(1, 21)]
        r._table.put_item(Item={"PK": control["PK"], "SK": "hotel-02", "target": targets[1], "callSid": CALL})
        selected_control, sids, selected_targets = send_report.batch_context(r._table, batch, targets)
        self.assertEqual(sids, [CALL])
        self.assertEqual([target["id"] for target in selected_targets], ["hotel-%02d" % n for n in range(2, 12)])
        self.assertEqual(send_report.integer_count(selected_control["attempted"]), 1)
        self.assertEqual(selected_control["eventRunId"], "hotel-trial-2026-10-15")
        with self.assertRaisesRegex(ValueError, "Unknown approved"):
            send_report.batch_context(r._table, "other-batch", targets)
        r._table.put_item(Item={"PK": control["PK"], "SK": "hotel-01", "target": targets[0], "callSid": "CA" + "9" * 32})
        with self.assertRaisesRegex(ValueError, "Unexpected batch target"):
            send_report.batch_context(r._table, batch, targets)

    def test_owner_rehearsal_batch_context_reports_takes_without_the_number(self):
        batch = "owner-rehearsal-2026-10-16"
        control = {"PK": "TRIAL#" + batch, "SK": "CONTROL", "status": "completed", "eventRunId": "hotel-trial-2026-10-15",
                   "attempted": Decimal("2"), "authorizedCalls": Decimal("2"), "spentUSD": Decimal("19")}
        r._table.put_item(Item=control)
        owner = {"id": "take-1", "bridgeId": "owner-rehearsal", "name": "Owner rehearsal take 1", "phone": "+15550001111"}
        r._table.put_item(Item={"PK": control["PK"], "SK": "take-1", "target": owner, "callSid": CALL})
        selected_control, sids, selected_targets = send_report.batch_context(r._table, batch, None)
        self.assertEqual(sids, [CALL])
        self.assertEqual(send_report.integer_count(selected_control["authorizedCalls"]), 2)
        self.assertEqual([t["id"] for t in selected_targets], ["owner-rehearsal"])
        self.assertNotIn("+1555", json.dumps(selected_targets))
        r._table.put_item(Item={"PK": control["PK"], "SK": "take-3", "target": {**owner, "id": "take-3"}, "callSid": "CA" + "8" * 32})
        with self.assertRaisesRegex(ValueError, "Unexpected batch target"):
            send_report.batch_context(r._table, batch, None)

    def test_transcript_attachment_contains_only_consented_turns_in_order(self):
        def event(second, kind, data):
            return {"createdAt": second, "data": {"callSid": CALL, "type": kind, "data": data}}
        report = {"runId": "batch", "calls": [{"callSid": CALL, "targetId": "hotel"}], "events": [
            event(4, "transcript", {"speaker": "assistant", "text": "Thank you"}),
            event(1, "transcript", {"speaker": "hotel", "text": "UNCONSENTED MENU"}),
            event(2, "consent", {"result": "granted"}),
            event(3, "transcript", {"speaker": "hotel", "text": "Password then accept"}),
            event(5, "transcript", {"speaker": "hotel", "text": "NAVIGATION", "phase": "navigation"}),
            event(6, "consent", {"result": "declined"}),
            event(7, "transcript", {"speaker": "hotel", "text": "AFTER WITHDRAWAL"})]}
        text = send_report.transcript_attachment(report, [{"id": "hotel", "name": "Real Hotel", "city": "California"}])
        self.assertIn("not guaranteed exact", text)
        self.assertIn("Real Hotel — California", text)
        self.assertNotIn("UNCONSENTED MENU", text)
        self.assertNotIn("NAVIGATION", text)
        self.assertNotIn("AFTER WITHDRAWAL", text)
        self.assertLess(text.index("Password then accept"), text.index("Thank you"))

    def test_offline_asr_attachment_is_bound_and_channels_stay_unverified(self):
        key = f"phone-trial/trial/{CALL}/{REC}.mp3"
        call = {"callSid": CALL, "recordingSid": REC, "runId": "trial", "s3Key": key,
                "recordingStatus": "ready", "consentStatus": "granted"}
        report = {"runId": "batch", "calls": [call], "events": []}
        artifact = {"callSid": CALL, "recordingSid": REC, "source": "offline_asr", "status": "completed", "channelAssignment": "unverified",
                    "channels": [{"channel": 1, "speaker": "unverified", "text": "Please hold", "status": "completed"},
                                 {"channel": 2, "speaker": "unverified", "text": "", "status": "no_speech"}]}
        s3 = unittest.mock.Mock()
        s3.get_object.return_value = {"Body": io.BytesIO(json.dumps(artifact).encode())}
        report["offlineTranscripts"] = send_report.offline_transcripts(s3, "private", report)
        s3.get_object.assert_called_once_with(Bucket="private", Key=key[:-4] + ".transcript.json")
        text = send_report.transcript_attachment(report)
        self.assertIn("Please hold", text)
        self.assertIn("Channel 1 (speaker unverified", text)
        self.assertIn("not a merged chronological", text)
        self.assertIn("No words recognized on this channel.", text)
        self.assertNotIn("hotel: Please hold", text)
        artifact["callSid"] = "CA" + "9" * 32
        s3.get_object.return_value = {"Body": io.BytesIO(json.dumps(artifact).encode())}
        result = send_report.offline_transcripts(s3, "private", report)
        self.assertEqual(result[CALL]["status"], "unavailable")
        self.assertEqual(result[CALL]["channels"], [])
        call["consentStatus"] = "declined"
        s3.get_object.reset_mock()
        self.assertEqual(send_report.offline_transcripts(s3, "private", report), {})
        s3.get_object.assert_not_called()

    def test_missing_offline_transcript_and_no_live_findings_are_honest(self):
        call = {"callSid": CALL, "recordingSid": REC, "runId": "trial", "s3Key": f"phone-trial/trial/{CALL}/{REC}.mp3",
                "recordingStatus": "ready", "consentStatus": "granted"}
        report = {"runId": "batch", "calls": [call], "events": []}
        s3 = unittest.mock.Mock()
        s3.get_object.side_effect = OSError("unavailable")
        report["offlineTranscripts"] = send_report.offline_transcripts(s3, "private", report)
        attachment = send_report.transcript_attachment(report)
        self.assertIn("Offline transcription is missing or could not be verified", attachment)
        _, body = send_report.build_report(report, "{{OUTCOME_ONE_LINE}}")
        self.assertIn("No completed interview or VPN findings are verified", body)


if __name__ == "__main__":
    unittest.main()
