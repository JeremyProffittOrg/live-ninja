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
            return {"usage_records": [{"price_unit": "usd", "price": getattr(self, "observed", "0")}]}
        if path == "/Calls/" + CALL + ".json" and form is None:
            self.lookups = getattr(self, "lookups", 0) + 1
            return {"sid": CALL, "account_sid": ACCOUNT, "status": "completed", "duration": "20",
                    "price": getattr(self, "call_price", None), "price_unit": "USD"}
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
        self.configure(budgetFloorUSD=Decimal("99.80"), budgetFloorMonth=m.budget_clock({})[0])
        reply = self.send()
        self.assertEqual(reply["statusCode"], 409)
        self.assertIn("monthly_cap_reached", json.loads(reply["body"])["error"])
        self.assertEqual(json.loads(reply["body"])["reason"], "monthly_cap_reached")
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
        self.configure(budgetFloorUSD=Decimal("99.80"))
        self.assertEqual(self.send()["statusCode"], 409)
        self.assertIsNone(r.get(m.request_key(OWNER, "request-one")))
        self.configure(budgetFloorUSD=Decimal("99.80"), budgetFloorMonth="2026-10")
        with patch.object(m, "budget_clock") as clock:
            clock.return_value = ("2026-10", "2026-10-31", "America/New_York", 9999999999)
            self.assertEqual(self.send()["statusCode"], 409)
            clock.return_value = ("2026-11", "2026-11-01", "America/New_York", 9999999999)
            self.assertEqual(self.send()["statusCode"], 200)
        budget = r.get({"PK": "PHONE#BUDGET#2026-11", "SK": "MONTH"})
        self.assertEqual(budget["allocatedUSD"], Decimal("8.30"))

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

    def month_row(self, allocated, **fields):
        month, _, _, _ = m.budget_clock(r.get(m.CONFIG_KEY))
        key = m.budget_keys(month, "")[0]
        r._table.put_item(Item={**key, "version": 7, "allocatedUSD": Decimal(allocated), "ledgerVersion": m.LEDGER_VERSION, "unsettled": {}, **fields})
        return key

    def test_monthly_cap_counts_every_call_up_to_one_hundred(self):
        self.setup_send()
        key = self.month_row("99.70")
        self.assertEqual(self.send()["statusCode"], 200)
        self.assertEqual(r.get(key)["allocatedUSD"], Decimal("100.00"))
        self.assertEqual(r.get(key)["unsettled"], {"request-one": Decimal("0.30")})
        self.callback("status", CallStatus="completed")
        reply = self.send(requestId="next")
        self.assertEqual(reply["statusCode"], 409)
        self.assertEqual(json.loads(reply["body"])["reason"], "monthly_cap_reached")
        self.assertEqual(len(self.forms), 1)
        self.assertEqual(m.budget_status(r.get(m.CONFIG_KEY))["pausedReason"], "monthly_cap_reached")

    def test_standing_daily_allowance_needs_no_grant_and_stops_at_ten(self):
        self.setup_send()
        cfg = r.get(m.CONFIG_KEY)
        month, day, zone, expires = m.budget_clock(cfg)
        day_key = m.budget_keys(month, day)[1]
        self.month_row("20")
        status = m.budget_status(cfg)
        self.assertEqual((status["dailyAllowanceUSD"], status["dailyRemainingUSD"], status["monthlyCapUSD"], status["pausedReason"]),
                         ("10", "10", "100", ""))
        self.assertEqual(self.send()["statusCode"], 200)
        self.assertEqual(r.get(day_key)["reservedUSD"], Decimal("0.30"))
        self.assertFalse(any(row["SK"].startswith("BUDGET_GRANT#") for row in r._table.items.values()))
        self.assertEqual(self.forms[0]["TimeLimit"], 90, "the per-call cap is unchanged")
        self.callback("status", CallStatus="completed")
        r._table.put_item(Item={**r.get(day_key), "reservedUSD": Decimal("9.80")})
        self.assertEqual(m.budget_status(cfg)["pausedReason"], "daily_allowance_used")
        reply = self.send(requestId="over")
        self.assertEqual(reply["statusCode"], 409)
        self.assertEqual(json.loads(reply["body"])["reason"], "daily_allowance_used")
        self.assertEqual(len(self.forms), 1)
        with patch.object(m, "budget_clock", return_value=(month, "2999-12-31", zone, expires + 86400)):
            tomorrow = m.budget_status(cfg)
        self.assertEqual((tomorrow["dailyReservedUSD"], tomorrow["dailyRemainingUSD"], tomorrow["pausedReason"]), ("0", "10", ""))

    def test_live_reservation_must_fit_the_daily_allowance(self):
        self.setup_send()
        cfg = r.get(m.CONFIG_KEY)
        month, day, zone, expires = m.budget_clock(cfg)
        self.month_row("20")
        # $0.40 left cannot fund the $0.459 telephone reserve: the model cost cap would be below zero.
        r._table.put_item(Item={**m.budget_keys(month, day)[1], "version": 1, "reservedUSD": Decimal("9.60"), "expiresAt": expires})
        with self.assertRaises(m.AllowanceRefused) as refused:
            m.reserve(OWNER, "live-one", "primary", "Live", cfg, telephone_reserve=m.TELEPHONE_RESERVE)
        self.assertEqual(refused.exception.reason, "daily_allowance_used")
        self.assertIsNone(r.get(m.request_key(OWNER, "live-one")))
        m.reserve(OWNER, "plain-one", "primary", "Plain", cfg, reserve_amount=m.RESERVE)
        self.assertEqual(r.get(m.budget_keys(month, day)[1])["reservedUSD"], Decimal("9.90"))
        self.assertEqual(self.forms, [])

    def test_live_cost_cap_is_the_remaining_allowance_less_the_telephone_reserve(self):
        self.setup_send()
        cfg = r.get(m.CONFIG_KEY)
        month, day, zone, expires = m.budget_clock(cfg)
        self.month_row("20")
        r._table.put_item(Item={**m.budget_keys(month, day)[1], "version": 1, "reservedUSD": Decimal("8.70"), "expiresAt": expires})
        item, fresh = m.reserve(OWNER, "live-two", "primary", "Live", cfg, telephone_reserve=m.TELEPHONE_RESERVE)
        self.assertTrue(fresh)
        self.assertEqual((item["reservedUSD"], item["costCapUSD"], item["telephoneReserveUSD"], item["maxCallSeconds"]),
                         (Decimal("1.30"), Decimal("0.841"), Decimal("0.459"), 600))
        self.assertEqual(r.get(m.budget_keys(month, day)[1])["reservedUSD"], Decimal("10"))
        self.assertEqual(r.get(m.budget_keys(month, "")[0])["unsettled"], {"live-two": Decimal("1.30")})
        self.assertEqual((m.view(item)["costCapUSD"], m.view(item)["maxCallSeconds"]), ("0.841000", 600))
        for bad in (Decimal("0"), Decimal("-1"), Decimal("10"), Decimal("NaN")):
            with self.assertRaises(ValueError):
                m.reserve(OWNER, "live-bad", "primary", "Live", cfg, telephone_reserve=bad)
        self.assertIsNone(r.get(m.request_key(OWNER, "live-bad")))

    def test_telephone_estimate_bills_started_minutes_plus_the_recording_charge(self):
        self.assertEqual(m.telephone_estimate(0), Decimal("0"))
        self.assertEqual(m.telephone_estimate(1), Decimal("0.2709"))
        self.assertEqual(m.telephone_estimate(60), Decimal("0.2709"))
        self.assertEqual(m.telephone_estimate(61), Decimal("0.2918"))
        self.assertEqual(m.telephone_estimate(600), Decimal("0.459"))
        self.assertEqual(m.TELEPHONE_RESERVE, m.telephone_estimate(m.MAX_CALL_SECONDS))
        self.assertEqual(m.cost_cap_string(Decimal("9.541")), "9.541000")
        self.assertEqual(m.parse_model_cost("0.000000"), Decimal("0"))
        self.assertEqual(m.parse_model_cost("10.000000"), m.PER_CALL_CAP, "the per-call cap itself is the largest valid model cost")
        for bad in ("1", "1.0", 1.0, None, "-0.000001", "1e2", "NaN", "10.000001", "12.345678", "9999.999999"):
            self.assertIsNone(m.parse_model_cost(bad), bad)

    def test_paused_reason_matches_the_owner_conversation_refusal_threshold(self):
        self.setup_send()
        cfg = r.get(m.CONFIG_KEY)
        month, day, zone, expires = m.budget_clock(cfg)
        day_key = m.budget_keys(month, day)[1]
        self.month_row("20")
        for reserved, expected in ((Decimal("9.50"), ""), (Decimal("9.541"), "daily_allowance_used"), (Decimal("9.60"), "daily_allowance_used")):
            with self.subTest(day_reserved=reserved):
                r._table.put_item(Item={**day_key, "version": 1, "reservedUSD": reserved, "expiresAt": expires})
                self.assertEqual(m.budget_status(cfg)["pausedReason"], expected)
        r._table.items.pop((day_key["PK"], day_key["SK"]))
        for allocated, expected in (("99.50", ""), ("99.541", "monthly_cap_reached"), ("99.60", "monthly_cap_reached")):
            with self.subTest(month_allocated=allocated):
                self.month_row(allocated)
                self.assertEqual(m.budget_status(cfg)["pausedReason"], expected)
        # $0.40 left: a conversation is refused, exactly as pausedReason says.
        with self.assertRaises(m.AllowanceRefused) as refused:
            m.reserve(OWNER, "live-paused", "primary", "Live", cfg, telephone_reserve=m.TELEPHONE_RESERVE)
        self.assertEqual(refused.exception.reason, "monthly_cap_reached")

    def test_prerecorded_message_keeps_its_ninety_second_cap_and_fixed_reserve(self):
        self.setup_send()
        cfg = r.get(m.CONFIG_KEY)
        month, day, _, _ = m.budget_clock(cfg)
        self.month_row("8")
        self.assertEqual(self.send()["statusCode"], 200)
        self.assertEqual(self.forms[0]["TimeLimit"], 90, "the prerecorded 90 s cap stays")
        item = r.get(m.request_key(OWNER, "request-one"))
        self.assertEqual(item["reservedUSD"], Decimal("0.30"))
        self.assertNotIn("costCapUSD", item); self.assertNotIn("maxCallSeconds", item); self.assertNotIn("deadline", item)
        self.assertEqual(r.get(m.budget_keys(month, day)[1])["reservedUSD"], Decimal("0.30"))
        self.assertNotIn("costCapUSD", json.loads(self.send()["body"]))
        status = m.budget_status(cfg)
        self.assertEqual((status["perCallCapUSD"], status["perCallMaxSeconds"], status["telephoneReserveUSD"]), ("10", 600, "0.4590"))

    def test_retired_grant_changes_nothing(self):
        self.setup_send()
        self.configure(enabled=False)
        r._table.put_item(Item={**m.VOICE_KEY, "activeRequest": "owner-stop"})
        before = copy.deepcopy(r._table.items)
        reply = m.budget_operation({"operation": "owner_budget_grant", "ownerUserId": OWNER, "requestId": "grant-stop",
                                    "expectedDay": m.budget_clock(r.get(m.CONFIG_KEY))[1]})
        self.assertEqual(reply["statusCode"], 410)
        self.assertEqual(r._table.items, before)
        self.assertEqual(m.budget_operation({"operation": "owner_budget_grant", "ownerUserId": OTHER})["statusCode"], 403)

    def test_version_one_month_is_rebased_to_floor_plus_measured_provider_cost(self):
        self.setup_send()
        self.observed = "4.60915"
        month, _, _, _ = m.budget_clock(r.get(m.CONFIG_KEY))
        key = m.budget_keys(month, "")[0]
        r._table.put_item(Item={**key, "version": 3, "allocatedUSD": Decimal("28.4")})
        status = m.budget_status(r.get(m.CONFIG_KEY))
        self.assertEqual(status["allocatedUSD"], "12.60915")
        row = r.get(key)
        self.assertEqual((row["ledgerVersion"], row["rebasedFromUSD"], row["unsettled"]), (2, Decimal("28.4"), {}))
        # A held voice lock means a reservation may be in flight: no rebase.
        r._table.put_item(Item={**key, "version": 3, "allocatedUSD": Decimal("28.4")})
        r._table.put_item(Item={**m.VOICE_KEY, "activeRequest": "owner:x"})
        self.assertEqual(m.budget_status(r.get(m.CONFIG_KEY))["allocatedUSD"], "28.4", "shown unchanged until it can be rebased")
        self.assertNotIn("ledgerVersion", r.get(key))

    def test_settlement_replaces_reservation_with_measured_cost_in_day_and_month(self):
        self.setup_send()
        cfg = r.get(m.CONFIG_KEY)
        month, day, _, _ = m.budget_clock(cfg)
        month_key, day_key = m.budget_keys(month, day)
        self.month_row("8")
        self.assertEqual(self.send()["statusCode"], 200)
        self.assertEqual(r.get(month_key)["allocatedUSD"], Decimal("8.30"))
        # Not terminal yet: no lookup, reservation kept.
        m.budget_status(cfg)
        self.assertEqual(getattr(self, "lookups", 0), 0)
        self.callback("status", CallStatus="completed")
        # Terminal with no provider price yet: the reservation stays.
        status = m.budget_status(cfg)
        self.assertEqual((status["allocatedUSD"], status["unsettledCalls"]), ("8.30", 1))
        self.assertEqual(r.get(day_key)["reservedUSD"], Decimal("0.30"))
        self.call_price = "-0.01300"
        status = m.budget_status(cfg)
        self.assertEqual((Decimal(status["allocatedUSD"]), status["unsettledCalls"]), (Decimal("8.013"), 0))
        self.assertEqual(r.get(day_key)["reservedUSD"], Decimal("0.013"))
        self.assertEqual(r.get(m.request_key(OWNER, "request-one"))["settledUSD"], Decimal("0.01300"))
        lookups = self.lookups
        m.budget_status(cfg)
        self.assertEqual(self.lookups, lookups, "a settled call is never charged twice")

    def test_live_settlement_needs_model_usage_and_never_drops_below_measured(self):
        self.setup_send()
        cfg = r.get(m.CONFIG_KEY)
        month, day, _, _ = m.budget_clock(cfg)
        month_key, day_key = m.budget_keys(month, day)
        self.month_row("8")
        self.assertEqual(self.send()["statusCode"], 200)
        key = m.request_key(OWNER, "request-one")
        item = r.get(key)
        # redeemedAt: the bridge received the bootstrap, so a missing model cost is unknown, not zero.
        r._table.put_item(Item={**item, "live": True, "providerStatus": "completed", "redeemedAt": 1000})
        r._table.put_item(Item={**m.VOICE_KEY})
        self.call_price = "-0.05"
        m.budget_status(cfg)
        self.assertEqual(r.get(month_key)["allocatedUSD"], Decimal("8.30"), "unknown model cost keeps the reservation")
        r._table.put_item(Item={**r.get(key), "inputTokens": 40000, "outputTokens": 10000})
        m.budget_status(cfg)
        self.assertEqual(r.get(month_key)["allocatedUSD"], Decimal("8.30"), "token counts without the bridge's modelCostUSD are not a measured cost")
        r._table.put_item(Item={**r.get(key), "modelCostUSD": Decimal("0.600000")})
        m.budget_status(cfg)
        # 20 s -> one started minute: 0.0209 + 0.25 = 0.2709 (above the 0.05 provider price), plus 0.60 model = 0.8709.
        self.assertEqual(r.get(month_key)["allocatedUSD"], Decimal("8.8709"))
        self.assertEqual(r.get(day_key)["reservedUSD"], Decimal("0.8709"))


if __name__ == "__main__":
    unittest.main()
