import base64
import copy
from decimal import Decimal
import hashlib
import hmac
import json
import os
import unittest
import urllib.parse
from unittest.mock import patch

import owner_messages as m
import recorder as r
import test_recorder as fixtures
from test_recorder import ACCOUNT, CALL, REC, Conflict

OWNER = "11111111-1111-4111-8111-111111111111"
OTHER = "22222222-2222-4222-8222-222222222222"


class OwnerMessageTests(unittest.TestCase):
    setUp = fixtures.RecorderTests.setUp
    tearDown = fixtures.RecorderTests.tearDown
    commit = fixtures.RecorderTests.commit

    def configure(self, **fields):
        cfg = {**m.CONFIG_KEY, "ownerUserId": OWNER, "enabled": True, "fromNumber": "+12025550100",
               "destinations": {"primary": "+12025550101", "secondary": "+12025550102"}, "budgetFloorUSD": Decimal("8"), **fields}
        r._table.put_item(Item=cfg)
        return cfg

    def transact(self, operations):
        before = copy.deepcopy(r._table.items)
        try:
            for operation in operations:
                if "Put" in operation:
                    value = operation["Put"]
                    item = value["Item"]
                    key = {k: item[k] for k in ("PK", "SK")}
                    old = r.get(key)
                    condition = value["ConditionExpression"]
                    if condition == "attribute_not_exists(PK)" and old:
                        raise Conflict()
                    if condition == "attribute_not_exists(activeRequest)" and (old or {}).get("activeRequest"):
                        raise Conflict()
                    if condition == "#v=:v" and (old or {}).get("version") != value["ExpressionAttributeValues"][":v"]:
                        raise Conflict()
                    r._table.put_item(Item=item)
                elif "ConditionCheck" in operation:
                    value = operation["ConditionCheck"]
                    cfg = r.get(value["Key"])
                    if value['ConditionExpression'] == 'attribute_not_exists(PK)':
                        if cfg:
                            raise Conflict()
                        continue
                    values = value["ExpressionAttributeValues"]
                    if value["ConditionExpression"].startswith("ownerUserId=:owner"):
                        if not cfg or cfg.get("ownerUserId") != values[":owner"]:
                            raise Conflict()
                        if (":zone" in values and cfg.get("timezone") != values[":zone"]) or (":zone" not in values and "timezone" in cfg):
                            raise Conflict()
                        continue
                    if not cfg or cfg.get("enabled") is not True or cfg["ownerUserId"] != values[":owner"] or cfg["fromNumber"] != values[":from"] or cfg["destinations"] != values[":destinations"]:
                        raise Conflict()
                    for name in ("budgetFloorUSD", "budgetFloorMonth", "timezone"):
                        if (":" + name in values and cfg.get(name) != values[":" + name]) or (":" + name not in values and name in cfg):
                            raise Conflict()
                else:
                    value = operation["Update"]
                    item = r.get(value["Key"])
                    if item.get("activeRequest") != value["ExpressionAttributeValues"][":request"]:
                        raise Conflict()
                    item.pop("activeRequest")
                    r._table.put_item(Item=item)
        except Exception:
            r._table.items = before
            raise

    def provider(self, path, form=None):
        if path.startswith("/Usage/"):
            return {"usage_records": [{"price_unit": "usd", "price": "0"}]}
        self.forms.append(form)
        return {"sid": CALL, "account_sid": ACCOUNT, "status": "queued"}

    def send(self, **fields):
        return r.handler({"operation": "owner_message_send", "ownerUserId": OWNER, "requestId": "request-one",
                          "destination": "primary", "message": "Hello & <test>", **fields}, None)

    def setup_send(self):
        self.configure()
        self.forms = []
        self.addCleanup(patch.stopall)
        patch.object(m, "transact", self.transact).start()
        patch.object(m, "provider", self.provider).start()

    def callback(self, kind, **fields):
        item = r.get(m.request_key(OWNER, "request-one"))
        path = "/owner-message/" + kind + "/" + item["callbackId"]
        data = {"AccountSid": ACCOUNT, "CallSid": CALL, **fields}
        signed = "https://api.example" + path + "".join(k + data[k] for k in sorted(data))
        signature = base64.b64encode(hmac.new(b"test-secret", signed.encode(), hashlib.sha1).digest()).decode()
        return r.handler({"rawPath": path, "requestContext": {"http": {"method": "POST"}}, "body": urllib.parse.urlencode(data), "headers": {"X-Twilio-Signature": signature}}, None)

    def test_identity_alias_and_http_send_fail_closed(self):
        self.setup_send()
        self.assertEqual(self.send(ownerUserId=OTHER)["statusCode"], 403)
        self.assertEqual(self.send(destination="+12025550103")["statusCode"], 400)
        self.assertEqual(self.send(message="x" * 1001)["statusCode"], 400)
        self.assertEqual(self.send(version="2.0")["statusCode"], 404)
        self.assertEqual(self.send(requestContext={})["statusCode"], 404)
        self.configure(enabled=False)
        self.assertEqual(self.send()["statusCode"], 403)
        self.assertEqual(self.forms, [])

    def test_idempotent_send_escaped_twiml_script_and_status(self):
        self.setup_send()
        reply = json.loads(self.send()["body"])
        self.assertTrue(reply["accepted"])
        self.assertFalse(reply["playbackCompleted"])
        self.assertEqual(reply["humanHeard"], "not_verified")
        self.assertEqual(reply["scriptTranscript"], m.INTRO + "Hello & <test>")
        self.assertEqual(reply["transcriptType"], "spoken_script_not_speech_recognition")
        self.assertIn("Hello &amp; &lt;test&gt;", self.forms[0]["Twiml"])
        self.assertEqual(self.forms[0]["TimeLimit"], 90)
        self.assertEqual(self.forms[0]["Record"], "true")
        self.assertEqual(self.send()["statusCode"], 200)
        self.assertEqual(len(self.forms), 1)
        self.assertEqual(self.send(message="different")["statusCode"], 409)
        self.assertEqual(r.handler({"operation": "owner_message_status", "ownerUserId": OTHER, "requestId": "request-one"}, None)["statusCode"], 403)
        rows = [v for v in r._table.items.values() if v["PK"].startswith("PHONE#BUDGET#")]
        self.assertEqual(rows[0]["allocatedUSD"], Decimal("8.30"))

    def test_recording_intent_precedes_provider_and_deleted_callback_cannot_copy(self):
        self.setup_send()
        def inspect(path, form=None):
            if path == '/Calls.json':
                self.assertEqual(r.get(m.request_key(OWNER, 'request-one'))['recordingCreateState'], 'attempted')
            return self.provider(path, form)
        with patch.object(m, 'provider', side_effect=inspect):
            self.send()
        call = r.get(r.call_key(CALL, m.namespace(OWNER)))
        self.assertEqual(call['recordingCreateState'], 'attempted')
        r._table.put_item(Item={'PK': 'OWNER#'+OWNER, 'SK': 'ACCOUNT_DELETION', 'ownerUserId': OWNER})
        with patch('account_lifecycle.resolve_deleted_recording', return_value=r.response(200)) as resolve, patch.object(r, 'copy_audio') as copy_audio:
            self.assertEqual(self.callback('recording', RecordingSid=REC, RecordingStatus='completed', To='+12025550999')['statusCode'], 409)
            resolve.assert_not_called()
            self.assertEqual(self.callback('recording', RecordingSid=REC, RecordingStatus='completed')['statusCode'], 200)
            self.assertEqual(resolve.call_args.args, (call, REC, 'completed'))
            copy_audio.assert_not_called()

    def test_revoked_recording_intent_never_reaches_call_creation(self):
        self.setup_send()
        original_change = m.change
        def revoke(key, fields, event_type, event_data, **kwargs):
            if event_type == 'recording_create_attempted':
                r._table.put_item(Item={'PK': 'OWNER#'+OWNER, 'SK': 'ACCOUNT_DELETION', 'ownerUserId': OWNER})
            return original_change(key, fields, event_type, event_data, **kwargs)
        with patch.object(m, 'change', side_effect=revoke):
            self.assertEqual(self.send()['statusCode'], 409)
        self.assertEqual(self.forms, [])
        self.assertNotIn('callSid', r.get(m.request_key(OWNER, 'request-one')))

    def test_unpersisted_intent_return_is_not_call_admission(self):
        self.setup_send()
        with patch.object(m, 'change', side_effect=lambda key, *args, **kwargs: r.get(key)):
            self.assertEqual(self.send()['statusCode'], 409)
        self.assertEqual(self.forms, [])

    def test_shared_batch_lock_and_budget_cap(self):
        self.setup_send()
        r._table.put_item(Item={**m.VOICE_KEY, "activeRequest": "batch:hotel"})
        self.assertEqual(self.send()["statusCode"], 409)
        self.assertIsNone(r.get(m.request_key(OWNER, "request-one")))
        self.assertEqual(self.forms, [])
        r._table.put_item(Item=m.VOICE_KEY)
        self.configure(budgetFloorUSD=Decimal("19.80"), budgetFloorMonth=m.datetime.now(m.timezone.utc).strftime("%Y-%m"))
        self.assertEqual(self.send()["statusCode"], 409)
        self.assertEqual(self.forms, [])

    def test_unknown_create_keeps_lock_and_never_redials(self):
        self.setup_send()
        def uncertain(path, form=None):
            if form:
                self.forms.append(form)
                raise m.ProviderError()
            return self.provider(path)
        with patch.object(m, "provider", uncertain):
            reply = json.loads(self.send()["body"])
            self.assertEqual(reply["providerStatus"], "unknown")
            self.assertFalse(reply["accepted"])
            self.send()
            self.assertEqual(len(self.forms), 1)
            self.assertEqual(self.send(requestId="second")["statusCode"], 409)
        self.assertTrue(r.get(m.VOICE_KEY)["activeRequest"])

    def test_definitive_rejection_releases_only_lock_and_retains_id(self):
        self.setup_send()
        def reject(path, form=None):
            if form:
                self.forms.append(form)
                raise m.ProviderError(21216, True)
            return self.provider(path)
        with patch.object(m, "provider", reject):
            reply = json.loads(self.send()["body"])
            self.assertEqual(reply["providerCode"], 21216)
            self.assertEqual(reply["providerStatus"], "rejected")
            self.send()
        self.assertEqual(len(self.forms), 1)
        self.assertNotIn("activeRequest", r.get(m.VOICE_KEY))

    def test_signed_callbacks_bind_before_create_returns_and_preserve_terminal(self):
        self.setup_send()
        def early(path, form=None):
            if form:
                self.assertEqual(self.callback("played")["statusCode"], 200)
                self.assertEqual(self.callback("status", CallStatus="completed")["statusCode"], 200)
            return self.provider(path, form)
        with patch.object(m, "provider", early):
            reply = json.loads(self.send()["body"])
        self.assertTrue(reply["playbackCompleted"])
        self.assertEqual(reply["providerStatus"], "completed")
        self.assertNotIn("activeRequest", r.get(m.VOICE_KEY))
        self.assertEqual(self.callback("status", CallStatus="ringing")["statusCode"], 200)
        self.assertEqual(r.get(m.request_key(OWNER, "request-one"))["providerStatus"], "completed")
        self.assertEqual(self.callback("status", CallStatus="completed", CallSid="CA" + "9" * 32)["statusCode"], 409)
        self.assertEqual(self.callback("status", CallStatus="completed", AccountSid="AC" + "9" * 32)["statusCode"], 400)
        self.assertEqual(self.callback("status", CallStatus="completed", To="+12025550104")["statusCode"], 409)

    def test_owner_recording_copy_and_private_download(self):
        self.setup_send()
        self.send()
        with patch.object(r, "copy_audio", return_value=("phone-trial/owner-audio.mp3", 100)) as copy_audio:
            self.assertEqual(self.callback("recording", RecordingSid=REC, RecordingStatus="completed")["statusCode"], 200)
            self.assertEqual(self.callback("recording", RecordingSid=REC, RecordingStatus="completed")["statusCode"], 200)
            self.assertEqual(copy_audio.call_count, 1)
        reply = json.loads(r.handler({"operation": "owner_message_status", "ownerUserId": OWNER, "requestId": "request-one"}, None)["body"])
        self.assertEqual(reply["recordingStatus"], "ready")
        token = urllib.parse.parse_qs(urllib.parse.urlsplit(reply["recordingLink"]["url"]).query)["token"][0]
        self.assertEqual(r.download({"token": token})["statusCode"], 302)

    def test_bad_signature_does_not_update_state(self):
        self.setup_send()
        self.send()
        item = r.get(m.request_key(OWNER, "request-one"))
        path = "/owner-message/status/" + item["callbackId"]
        self.assertEqual(m.callback(path, b"AccountSid=x", {"x-twilio-signature": "bad"})["statusCode"], 403)
        self.assertEqual(r.get(m.request_key(OWNER, "request-one")), item)

    def test_terminal_status_releases_only_own_lock_and_no_answer_is_absent(self):
        self.setup_send()
        self.send()
        r._table.put_item(Item={**m.VOICE_KEY, "activeRequest": "batch:other"})
        self.assertEqual(self.callback("status", CallStatus="no-answer")["statusCode"], 200)
        self.assertEqual(r.get(m.VOICE_KEY)["activeRequest"], "batch:other")
        call = r.get(r.call_key(CALL, m.namespace(OWNER)))
        self.assertEqual(call["recordingStatus"], "absent")
        self.assertEqual(call["recordingReason"], "call_no-answer")

    def test_budget_preserves_existing_higher_allocation(self):
        self.setup_send()
        month = m.datetime.now(m.timezone.utc).strftime("%Y-%m")
        key = {"PK": "PHONE#BUDGET#" + month, "SK": "MONTH"}
        r._table.put_item(Item={**key, "version": 7, "allocatedUSD": Decimal("19.70")})
        self.assertEqual(self.send()["statusCode"], 200)
        self.assertEqual(r.get(key)["allocatedUSD"], Decimal("20.00"))
        self.callback("status", CallStatus="completed")
        self.assertEqual(self.send(requestId="next")["statusCode"], 409)
        self.assertEqual(len(self.forms), 1)
        self.assertEqual(r.get(key)["version"], 8)

    def test_provider_usage_unavailable_never_reserves_or_calls(self):
        self.setup_send()
        with patch.object(m, "provider", return_value={"usage_records": []}):
            self.assertEqual(self.send()["statusCode"], 409)
        self.assertIsNone(r.get(m.request_key(OWNER, "request-one")))
        self.assertIsNone(r.get(m.VOICE_KEY))

    def test_owner_recording_failure_retries_copy_without_redial(self):
        self.setup_send()
        self.send()
        with patch.object(r, "copy_audio", side_effect=OSError("provider unavailable")):
            self.assertEqual(self.callback("recording", RecordingSid=REC, RecordingStatus="completed")["statusCode"], 503)
        self.assertEqual(r.get(r.call_key(CALL, m.namespace(OWNER)))["recordingStatus"], "failed")
        with patch.object(r, "copy_audio", return_value=("phone-trial/owner-audio.mp3", 100)) as fetch:
            self.assertEqual(self.callback("recording", RecordingSid=REC, RecordingStatus="completed")["statusCode"], 503)
            fetch.assert_not_called()
            self.now.return_value = 1030
            self.assertEqual(self.callback("recording", RecordingSid=REC, RecordingStatus="completed")["statusCode"], 200)
        self.assertEqual(r.get(r.call_key(CALL, m.namespace(OWNER)))["recordingStatus"], "ready")
        self.assertEqual(len(self.forms), 1)

    def test_budget_floor_month_rollover_and_missing_month(self):
        self.setup_send()
        self.configure(budgetFloorUSD=Decimal("19.80"))
        self.assertEqual(self.send()["statusCode"], 409)
        self.assertIsNone(r.get(m.request_key(OWNER, "request-one")))
        self.configure(budgetFloorUSD=Decimal("19.80"), budgetFloorMonth="2026-10")
        with patch.object(m, "budget_clock") as clock:
            clock.return_value = ("2026-10", "2026-10-31", "America/New_York", 9999999999)
            self.assertEqual(self.send()["statusCode"], 409)
            clock.return_value = ("2026-11", "2026-11-01", "America/New_York", 9999999999)
            self.assertEqual(self.send()["statusCode"], 200)
        budget = r.get({"PK": "PHONE#BUDGET#2026-11", "SK": "MONTH"})
        self.assertEqual(budget["allocatedUSD"], Decimal("8.30"))

    def test_explicit_daily_grant_can_cover_larger_call_reservation(self):
        self.setup_send()
        cfg = r.get(m.CONFIG_KEY)
        month, day, zone, expiry = m.budget_clock(cfg)
        day_key = m.budget_keys(month, day)[1]
        for index, remaining in enumerate(('0.30', '1.39', '1.40')):
            r._table.put_item(Item={**day_key, 'version': 1, 'grantedUSD': Decimal('5'),
                'spentUSD': Decimal('5')-Decimal(remaining), 'expiresAt': expiry, 'timezone': zone})
            event = {'operation': 'owner_budget_grant', 'ownerUserId': OWNER, 'requestId': 'bound-'+str(index), 'expectedDay': day}
            reply = r.handler(event, None)
            self.assertEqual(reply['statusCode'], 200 if Decimal(remaining)<Decimal('1.40') else 409)
            if reply['statusCode'] == 200:
                self.assertEqual(json.loads(reply['body'])['requiredReservationUSD'], '1.40')
                self.assertEqual(r._table.get_item(Key=day_key)['Item']['grantedUSD'], Decimal('10'))
                r.handler(event, None)
                self.assertEqual(r._table.get_item(Key=day_key)['Item']['grantedUSD'], Decimal('10'))

    def test_provider_completed_is_not_playback_completed(self):
        self.setup_send()
        self.send()
        self.callback("status", CallStatus="completed")
        item = r.get(m.request_key(OWNER, "request-one"))
        self.assertFalse(m.view(item)["playbackCompleted"])
        self.assertLess(self.forms[0]["Twiml"].index("</Say>"), self.forms[0]["Twiml"].index("<Redirect"))
        reply = self.callback("played")
        self.assertEqual(reply["body"], "<Response><Hangup/></Response>")
        self.assertTrue(m.view(r.get(m.request_key(OWNER, "request-one")))["playbackCompleted"])

    def test_early_answer_is_not_overwritten_by_queued_create_response(self):
        self.setup_send()
        def early_answer(path, form=None):
            if form:
                self.assertEqual(self.callback("status", CallStatus="in-progress")["statusCode"], 200)
            return self.provider(path, form)
        with patch.object(m, "provider", early_answer):
            reply = json.loads(self.send()["body"])
        self.assertEqual(reply["providerStatus"], "in-progress")
        self.callback("status", CallStatus="ringing")
        self.assertEqual(r.get(m.request_key(OWNER, "request-one"))["providerStatus"], "in-progress")

    def grant(self, **fields):
        return m.budget_operation({"operation": "owner_budget_grant", "ownerUserId": OWNER,
            "requestId": "grant-one", "expectedDay": m.budget_clock(self.configure())[1], **fields})

    def test_grant_replay_and_reservation_charge_once(self):
        self.setup_send()
        month, day, _, _ = m.budget_clock(self.configure())
        month_key, day_key = m.budget_keys(month, day)
        r._table.put_item(Item={**month_key, "allocatedUSD": Decimal("20"), "version": 1})
        self.assertEqual(self.send()["statusCode"], 409)
        self.assertEqual(self.grant()["statusCode"], 200)
        self.assertEqual(self.grant()["statusCode"], 200)
        self.assertEqual(self.grant(requestId="prestack")["statusCode"], 409)
        self.assertEqual(r.get(day_key)["grantedUSD"], Decimal("5"))
        self.assertEqual(self.send()["statusCode"], 200)
        self.assertEqual(self.send()["statusCode"], 200)
        self.assertEqual(r.get(day_key)["spentUSD"], Decimal("0.30"))
        self.assertEqual(len(self.forms), 1)

    def test_budget_status_reports_crossing_reservation_hold(self):
        self.setup_send()
        cfg = self.configure()
        month, day, _, expires = m.budget_clock(cfg)
        month_key, day_key = m.budget_keys(month, day)
        r._table.put_item(Item={**month_key, "allocatedUSD": Decimal("19.80"), "version": 1})
        self.assertEqual(m.budget_status(cfg)["pausedReason"], "waiting_approval")
        self.assertEqual(self.send()["statusCode"], 409)
        self.assertEqual(self.forms, [])
        r._table.put_item(Item={**day_key, "grantedUSD": Decimal("5"), "spentUSD": Decimal("4.91"), "version": 1, "expiresAt": expires})
        self.assertEqual(m.budget_status(cfg)["pausedReason"], "waiting_approval")
        r._table.put_item(Item={**day_key, "grantedUSD": Decimal("5"), "spentUSD": Decimal("4.90"), "version": 2, "expiresAt": expires})
        self.assertEqual(m.budget_status(cfg)["pausedReason"], "")

    def test_expired_grant_and_exhaustion_fail_closed(self):
        self.setup_send()
        month, day, _, expires = m.budget_clock(self.configure())
        month_key, day_key = m.budget_keys(month, day)
        r._table.put_item(Item={**month_key, "allocatedUSD": Decimal("20"), "version": 1})
        r._table.put_item(Item={**day_key, "grantedUSD": Decimal("5"), "spentUSD": Decimal("4.80"), "version": 1, "expiresAt": expires})
        self.assertEqual(self.send()["statusCode"], 409)
        self.grant()
        old = r.get(day_key)
        r._table.put_item(Item={**old, "expiresAt": 1})
        self.assertEqual(self.send()["statusCode"], 409)
        self.assertEqual(self.forms, [])

    def test_grant_does_not_clear_owner_stop_or_enable_calls(self):
        self.setup_send()
        self.configure(enabled=False)
        r._table.put_item(Item={**m.VOICE_KEY, "activeRequest": "owner-stop"})
        event = {"operation": "owner_budget_grant", "ownerUserId": OWNER, "requestId": "grant-stop",
                 "expectedDay": m.budget_clock(r.get(m.CONFIG_KEY))[1]}
        self.assertEqual(m.budget_operation(event)["statusCode"], 200)
        self.assertFalse(r.get(m.CONFIG_KEY)["enabled"])
        self.assertEqual(self.send()["statusCode"], 403)
        self.configure(enabled=True)
        self.assertEqual(self.send()["statusCode"], 409)
        self.assertEqual(r.get(m.VOICE_KEY)["activeRequest"], "owner-stop")
        self.assertEqual(self.forms, [])

    def test_grant_owner_day_binding_and_concurrent_retry(self):
        self.setup_send()
        self.assertEqual(self.grant(ownerUserId=OTHER)["statusCode"], 403)
        self.assertEqual(self.grant(expectedDay="2000-01-01")["statusCode"], 409)
        calls = []
        def competing(operations):
            calls.append(1)
            if len(calls) == 1:
                # Another approval wins the same day-row CAS before this transaction.
                item = copy.deepcopy(operations[0]["Put"]["Item"])
                r._table.put_item(Item=item)
                raise Conflict()
            return self.transact(operations)
        with patch.object(m, "transact", competing):
            self.assertEqual(self.grant()["statusCode"], 409)
        status = m.budget_status(r.get(m.CONFIG_KEY))
        self.assertEqual(status["dailyGrantedUSD"], "5")
        self.assertEqual(len(calls), 1)

    def test_local_day_month_and_dst_expiry(self):
        from datetime import datetime
        for instant, month, day, hours in (("2026-11-01T00:00:00", "2026-11", "2026-11-01", 25),
                                         ("2026-03-08T00:00:00", "2026-03", "2026-03-08", 23)):
            now = datetime.fromisoformat(instant).replace(tzinfo=m.ZoneInfo("America/New_York"))
            with patch.object(m, "datetime", wraps=datetime) as clock:
                clock.now.return_value = now
                result = m.budget_clock({})
            self.assertEqual(result[:3], (month, day, "America/New_York"))
            self.assertEqual(result[3]-int(now.timestamp()), hours*3600)

    def test_midnight_expires_authority_and_replay_cannot_extend_it(self):
        self.setup_send()
        with patch.object(m, "budget_clock", return_value=("2026-10", "2026-10-30", "America/New_York", 9999999999)):
            self.assertEqual(self.grant()["statusCode"], 200)
        with patch.object(m, "budget_clock", return_value=("2026-10", "2026-10-31", "America/New_York", 9999999999)):
            self.assertEqual(self.grant()["statusCode"], 409)
            status = m.budget_status(r.get(m.CONFIG_KEY))
            self.assertEqual(status["dailyRemainingUSD"], "0")
            month_key, _ = m.budget_keys("2026-10", "2026-10-31")
            r._table.put_item(Item={**month_key, "allocatedUSD": Decimal("20"), "version": 1})
            self.assertEqual(self.send()["statusCode"], 409)
        self.assertEqual(self.forms, [])

    def test_daily_call_duration_stays_inside_midnight_authority(self):
        self.setup_send()
        month, day, _, _ = m.budget_clock(self.configure())
        month_key, day_key = m.budget_keys(month, day)
        r._table.put_item(Item={**month_key, "allocatedUSD": Decimal("20"), "version": 1})
        r._table.put_item(Item={**day_key, "grantedUSD": Decimal("5"), "spentUSD": Decimal("0"), "version": 1, "expiresAt": 1060})
        with patch.object(m.time, "time", return_value=1000):
            self.assertEqual(self.send()["statusCode"], 200)
        self.assertEqual(self.forms[0]["TimeLimit"], 30)

    def test_grant_rejects_concurrent_timezone_change(self):
        self.setup_send()
        def change_zone(operations):
            self.configure(timezone="America/Los_Angeles")
            return self.transact(operations)
        with patch.object(m, "transact", change_zone):
            self.assertEqual(self.grant()["statusCode"], 409)
        self.assertFalse(any(row["SK"].startswith("BUDGET_GRANT#") for row in r._table.items.values()))


if __name__ == "__main__":
    unittest.main()
