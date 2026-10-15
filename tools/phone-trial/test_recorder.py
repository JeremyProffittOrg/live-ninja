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

    def put_object(self, **kwargs):
        assert kwargs["ServerSideEncryption"] == "AES256"
        self.data = kwargs["Body"].read()

    def head_object(self, **kwargs):
        return {"ContentLength": len(self.data)}

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
        self.clock.start()

    def tearDown(self):
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

    def test_wrong_account_or_unregistered_call_rejected(self):
        self.assertEqual(self.callback()["statusCode"], 409)
        self.register()
        self.assertEqual(self.callback(AccountSid="AC" + "4" * 32)["statusCode"], 400)

    def test_canonical_audio_url_ignores_callback_url_and_becomes_ready(self):
        self.register()
        class Opener:
            def open(inner, request, timeout):
                self.assertEqual(request.full_url, f"https://api.twilio.com/2010-04-01/Accounts/{ACCOUNT}/Recordings/{REC}.mp3")
                self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], "pending")
                return Audio()
        with patch.object(r.urllib.request, "build_opener", return_value=Opener()):
            self.assertEqual(self.callback(RecordingUrl="http://169.254.169.254/")["statusCode"], 200)
        self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], "ready")
        self.assertEqual(r._s3.data, b"MP3")
        with patch.object(r, "copy_audio", side_effect=AssertionError("duplicate fetched")):
            self.assertEqual(self.callback()["statusCode"], 200)
        self.assertEqual(self.callback(RecordingSid="RE" + "5" * 32)["statusCode"], 409)

    def test_missing_audio_and_failed_copy_are_visible(self):
        self.register()
        self.assertEqual(self.callback(RecordingStatus="absent")["statusCode"], 200)
        self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], "absent")
        with patch.object(r, "copy_audio", side_effect=OSError("provider unavailable")):
            self.assertEqual(self.callback()["statusCode"], 503)
        self.assertEqual(r.get(r.call_key(CALL))["recordingStatus"], "failed")

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


if __name__ == "__main__":
    unittest.main()
