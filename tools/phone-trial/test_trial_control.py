"""Offline checks of the actual call runner's side-effect boundaries."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import re
import sys
import types
import unittest
from unittest.mock import patch

sys.modules.setdefault("boto3", types.ModuleType("boto3"))
import run_trial
READ_TEXT = Path.read_text
ENSURE_WINDOW = run_trial.ensure_window


class Table:
    def __init__(self):
        self.rows = {}

    def get_item(self, Key, **kwargs):
        row = self.rows.get((Key["PK"], Key["SK"]))
        return {"Item": dict(row)} if row else {}

    def put_item(self, Item, **kwargs):
        key = (Item["PK"], Item["SK"])
        if key in self.rows and kwargs.get("ConditionExpression"):
            raise RuntimeError("Duplicate conditional write")
        self.rows[key] = dict(Item)

    def update_item(self, Key, UpdateExpression, ExpressionAttributeValues, ExpressionAttributeNames=None, **kwargs):
        row = self.rows.setdefault((Key["PK"], Key["SK"]), dict(Key))
        condition = kwargs.get("ConditionExpression", "")
        if condition == "attribute_not_exists(activeRequest)" and "activeRequest" in row:
            raise RuntimeError("Lock held")
        if condition == "activeRequest=:owner" and row.get("activeRequest") != ExpressionAttributeValues[":owner"]:
            raise RuntimeError("Wrong lock owner")
        if UpdateExpression == "REMOVE activeRequest":
            row.pop("activeRequest", None)
            return
        names = ExpressionAttributeNames or {}
        for assignment in UpdateExpression.removeprefix("SET ").split(","):
            key, value = [v.strip() for v in assignment.split("=")]
            row[names.get(key, key)] = ExpressionAttributeValues[value]

    def query(self, **kwargs):
        return {"Items": [v for (pk, sk), v in self.rows.items() if pk == kwargs["ExpressionAttributeValues"][":pk"]]}


class RunnerTests(unittest.TestCase):
    def setUp(self):
        self.table = Table()
        self.created = []
        self.month = run_trial.Decimal("2")
        self.fail_create = False
        self.provider_status = "completed"
        self.targets = [{"id": "hotel-%02d" % (i + 1), "name": "Synthetic Hotel", "phone": "+1555000" + str(1000 + i), "city": "Test", "source": "https://example.invalid"} for i in range(20)]
        self.patches = [
            patch.dict(run_trial.os.environ, {"GITHUB_ACTIONS": "true", "EXECUTE_PHONE_TRIAL": run_trial.RUN, "PHONE_TRIAL_BATCH": run_trial.RUN, "BRIDGE_URL": "https://example.invalid"}),
            patch.object(run_trial.Path, "read_text", return_value=json.dumps({"hotels": self.targets})),
            patch.object(run_trial, "ensure_window"), patch.object(run_trial, "select_caller", return_value="+15559990000"),
            patch.object(run_trial, "existing_month_cost", side_effect=lambda: self.month),
            patch.object(run_trial, "event"), patch.object(run_trial.time, "sleep"),
            patch.object(run_trial, "invoke", return_value={"events": [], "calls": []}),
            patch.object(run_trial, "twilio", side_effect=self.twilio),
            patch.object(run_trial.boto3, "resource", create=True, return_value=types.SimpleNamespace(Table=lambda _: self.table)),
        ]
        for p in self.patches:
            p.start()
            self.addCleanup(p.stop)

    def twilio(self, path, method="GET", form=None):
        if path == "/Calls.json":
            if self.fail_create:
                raise RuntimeError("Ambiguous create timeout")
            self.created.append(dict(form))
            return {"sid": "CA" + str(len(self.created)).zfill(32), "status": self.provider_status, "duration": "10"}
        if path.endswith("/Recordings.json"):
            return {"recordings": []}
        return {"status": "completed", "duration": "10"}

    def run_silently(self):
        with contextlib.redirect_stdout(io.StringIO()):
            run_trial.main()

    def test_twenty_bounded_calls_no_record_before_consent(self):
        self.run_silently()
        self.assertEqual(len(self.created), 20)
        self.assertEqual(len({x["To"] for x in self.created}), 20)
        self.assertTrue(all(x["TimeLimit"] == 180 and "Record" not in x for x in self.created))
        self.run_silently()
        self.assertEqual(len(self.created), 20, "Completed trial must never redial")

    def test_monthly_limit_prevents_call_creation(self):
        self.month = run_trial.Decimal("20")
        with self.assertRaises(SystemExit):
            self.run_silently()
        self.assertEqual(self.created, [])

    def test_failed_provider_canary_stops_remaining_destinations(self):
        self.provider_status = "failed"
        with self.assertRaises(SystemExit):
            self.run_silently()
        self.assertEqual(len(self.created), 1)

    def test_stream_rejection_stops_remaining_destinations(self):
        report = {"calls": [], "events": [{"SK": "error", "data": {"callSid": "CA" + str(1).zfill(32), "type": "call_ended", "data": {"reason": "stream_rejected"}}}]}
        with patch.object(run_trial, "invoke", return_value=report):
            with self.assertRaises(SystemExit):
                self.run_silently()
        self.assertEqual(len(self.created), 1)

    def test_ambiguous_creation_blocks_automatic_retry(self):
        self.fail_create = True
        with self.assertRaises(SystemExit):
            self.run_silently()
        self.fail_create = False
        with self.assertRaises(SystemExit):
            self.run_silently()
        self.assertEqual(self.created, [])

    def test_verified_empty_provider_history_allows_uncreated_intent(self):
        self.fail_create = True
        with self.assertRaises(SystemExit):
            self.run_silently()
        self.fail_create = False
        original = self.twilio
        def reconciled(path, method="GET", form=None):
            if path.startswith("/Calls.json?"):
                return {"calls": [], "next_page_uri": None}
            return original(path, method, form)
        with patch.object(run_trial, "twilio", side_effect=reconciled):
            self.run_silently()
        self.assertEqual(len(self.created), 20)
        first = self.table.rows[("TRIAL#" + run_trial.RUN, self.targets[0]["id"])]
        self.assertIn("noPriorCallVerifiedAt", first)

    def test_missing_execution_flag_has_no_side_effect(self):
        with patch.dict(run_trial.os.environ, {"EXECUTE_PHONE_TRIAL": ""}):
            with self.assertRaises(SystemExit):
                self.run_silently()
        self.assertEqual(self.table.rows, {})

    def test_usage_duplicate_events_not_double_charged(self):
        entry = {"SK": "event", "data": {"callSid": "CA1", "type": "usage", "data": {"input_tokens": 1000, "output_tokens": 500}}}
        self.assertEqual(run_trial.usage_charge([entry, entry], "CA1"), run_trial.Decimal("0.020"))

    def ten_batch(self):
        return patch.dict(run_trial.os.environ, {"PHONE_TRIAL_BATCH": "hotel-vpn-ten-2026-10-16",
                          "EXECUTE_PHONE_TRIAL": "hotel-vpn-ten-2026-10-16"})

    def test_ten_batch_has_distinct_ledger_same_event_namespace(self):
        with self.ten_batch():
            self.run_silently()
            self.run_silently()
        self.assertEqual(len(self.created), 10)
        self.assertEqual({x["To"] for x in self.created}, {t["phone"] for t in self.targets[1:11]})
        self.assertTrue(all("run=" + run_trial.RUN in x["Url"] for x in self.created))
        control = self.table.rows[("TRIAL#hotel-vpn-ten-2026-10-16", "CONTROL")]
        self.assertEqual(control["authorizedCalls"], 10)
        self.assertEqual(control["authorizedDate"], "2026-10-16")
        self.assertEqual(control["eventRunId"], run_trial.RUN)
        self.assertNotIn("activeRequest", self.table.rows[("PHONE#CONTROL", "VOICE")])
        run_trial.ensure_window.assert_called_with("2026-10-16", 0)

    def test_october_sixteen_window_starts_immediately_and_rejects_other_dates(self):
        with patch.object(run_trial, "datetime") as clock, patch.object(run_trial, "ZoneInfo", return_value=run_trial.timezone.utc):
            clock.now.return_value = types.SimpleNamespace(date=lambda: types.SimpleNamespace(isoformat=lambda: "2026-10-16"), hour=1)
            ENSURE_WINDOW("2026-10-16", 0)
            run_trial.time.sleep.assert_not_called()
            clock.now.return_value = types.SimpleNamespace(date=lambda: types.SimpleNamespace(isoformat=lambda: "2026-10-17"), hour=1)
            with self.assertRaises(RuntimeError):
                ENSURE_WINDOW("2026-10-16", 0)

    def test_wrong_batch_execution_guard_blocks_writes(self):
        with patch.dict(run_trial.os.environ, {"PHONE_TRIAL_BATCH": "hotel-vpn-ten-2026-10-16"}):
            with self.assertRaises(SystemExit):
                self.run_silently()
        self.assertEqual(self.table.rows, {})

    def test_other_voice_owner_prevents_calls(self):
        self.table.rows[("PHONE#CONTROL", "VOICE")] = {"PK": "PHONE#CONTROL", "SK": "VOICE", "activeRequest": "owner-call"}
        with self.ten_batch(), self.assertRaises(SystemExit):
            self.run_silently()
        self.assertEqual(self.created, [])
        self.assertEqual(self.table.rows[("PHONE#CONTROL", "VOICE")]["activeRequest"], "owner-call")

    def test_unknown_ten_batch_creation_keeps_lock_and_does_not_redial(self):
        self.fail_create = True
        with self.ten_batch():
            with self.assertRaises(SystemExit):
                self.run_silently()
            self.fail_create = False
            with self.assertRaises(SystemExit):
                self.run_silently()
        self.assertEqual(self.created, [])
        self.assertEqual(self.table.rows[("PHONE#CONTROL", "VOICE")]["activeRequest"], "batch:hotel-vpn-ten-2026-10-16")

    def test_ten_batch_carries_prior_budget_and_pending_reserve(self):
        self.table.rows[("TRIAL#" + run_trial.RUN, "CONTROL")] = {"PK": "TRIAL#" + run_trial.RUN, "SK": "CONTROL", "spentUSD": run_trial.Decimal("18.50")}
        self.table.rows[("TRIAL#" + run_trial.RUN, "hotel-01")] = {"PK": "TRIAL#" + run_trial.RUN, "SK": "hotel-01", "settled": False, "reserveUSD": run_trial.CALL_RESERVE}
        with self.ten_batch(), self.assertRaises(SystemExit):
            self.run_silently()
        self.assertEqual(self.created, [])
        self.assertEqual(self.table.rows[("TRIAL#hotel-vpn-ten-2026-10-16", "CONTROL")]["spentUSD"], run_trial.Decimal("19.90"))
        self.assertNotIn("activeRequest", self.table.rows[("PHONE#CONTROL", "VOICE")])

    def test_consent_declined_is_terminal_nontechnical_result(self):
        report = {"calls": [], "events": [{"SK": "end", "data": {"callSid": "CA" + str(1).zfill(32), "type": "call_ended", "data": {"reason": "consent_declined"}}}]}
        with self.ten_batch(), patch.object(run_trial, "invoke", return_value=report):
            self.run_silently()
        self.assertEqual(len(self.created), 10)

    OWNER = "+15550002222"

    def owner_batch(self, takes="3", number=None):
        return patch.dict(run_trial.os.environ, {"PHONE_TRIAL_BATCH": run_trial.OWNER_BATCH, "EXECUTE_PHONE_TRIAL": run_trial.OWNER_BATCH,
                          "OWNER_REHEARSAL_NUMBER": self.OWNER if number is None else number, "OWNER_REHEARSAL_TAKES": takes})

    def test_owner_rehearsal_dials_only_the_secret_number_sequentially(self):
        with self.owner_batch(), patch.object(run_trial, "ensure_date") as dated:
            self.run_silently()
            self.run_silently()
        self.assertEqual(len(self.created), 3, "At most three takes and never a redial")
        self.assertEqual({x["To"] for x in self.created}, {self.OWNER})
        self.assertTrue(all("target=owner-rehearsal" in x["Url"] and "run=" + run_trial.RUN in x["Url"] for x in self.created))
        self.assertTrue(all(x["TimeLimit"] == 180 and "Record" not in x for x in self.created))
        run_trial.ensure_window.assert_not_called()
        dated.assert_called_with(run_trial.OWNER_DATE)
        pauses = [c.args[0] for c in run_trial.time.sleep.call_args_list if c.args and c.args[0] == run_trial.OWNER_TAKE_PAUSE]
        self.assertEqual(len(pauses), 2, "45-second pause only between takes")
        control = self.table.rows[("TRIAL#" + run_trial.OWNER_BATCH, "CONTROL")]
        self.assertEqual((control["status"], control["authorizedCalls"], control["attempted"]), ("completed", 3, 3))
        self.assertNotIn("activeRequest", self.table.rows[("PHONE#CONTROL", "VOICE")])

    def test_owner_rehearsal_takes_input_bounds_calls(self):
        with self.owner_batch(takes="1"), patch.object(run_trial, "ensure_date"):
            self.run_silently()
        self.assertEqual(len(self.created), 1)
        for bad in ("0", "4", "", "3; rm"):
            self.setUp()
            with self.owner_batch(takes=bad), patch.object(run_trial, "ensure_date"), self.assertRaises(SystemExit):
                self.run_silently()
            self.assertEqual(self.created, [])

    def test_owner_rehearsal_refuses_missing_or_other_numbers(self):
        for bad in ("", "5550002222", "+44 20 7946 0000", "+1555000100x"):
            with self.owner_batch(number=bad), patch.object(run_trial, "ensure_date"), self.assertRaises(SystemExit):
                self.run_silently()
            self.assertEqual(self.created, [])
        hotel = self.targets[0]
        with self.owner_batch(), patch.object(run_trial, "ensure_date"),                 patch.object(run_trial, "owner_targets", return_value=[{**hotel, "id": "take-1", "bridgeId": "owner-rehearsal"}]),                 self.assertRaises(SystemExit):
            self.run_silently()
        self.assertEqual(self.created, [])

    def test_owner_decline_or_hangup_still_runs_next_take(self):
        def report(payload):
            return {"calls": [], "events": [{"SK": "e" + str(len(self.created)), "data": {"callSid": "CA" + str(len(self.created)).zfill(32), "type": "call_ended",
                    "data": {"reason": "consent_declined" if len(self.created) == 1 else "remote_hangup"}}}]}
        with self.owner_batch(), patch.object(run_trial, "ensure_date"), patch.object(run_trial, "invoke", side_effect=report):
            self.run_silently()
        self.assertEqual(len(self.created), 3)

    def test_owner_rehearsal_respects_shared_budget_and_threshold(self):
        self.table.rows[("TRIAL#" + run_trial.TEN_BATCH, "CONTROL")] = {"PK": "TRIAL#" + run_trial.TEN_BATCH, "SK": "CONTROL", "spentUSD": run_trial.Decimal("17.78806")}
        month = run_trial.month_budget_key()
        self.table.rows[(month["PK"], month["SK"])] = {**month, "allocatedUSD": run_trial.Decimal("18.30"), "version": 2}
        with self.owner_batch(), patch.object(run_trial, "ensure_date"), self.assertRaises(SystemExit):
            self.run_silently()
        # Each unconsented 10-second take settles at 0.2309: 18.53 and 18.76 leave room for one more
        # 1.40 reservation only once, so take three is refused before dialing.
        self.assertEqual(len(self.created), 2)
        control = self.table.rows[("TRIAL#" + run_trial.OWNER_BATCH, "CONTROL")]
        self.assertEqual(control["status"], "blocked")
        self.assertIn("$20 threshold", control["detail"])
        self.assertEqual(self.table.rows[(month["PK"], month["SK"])]["allocatedUSD"], control["spentUSD"])
        self.table.rows[(month["PK"], month["SK"])]["allocatedUSD"] = run_trial.Decimal("19")
        self.table.rows.pop(("TRIAL#" + run_trial.OWNER_BATCH, "CONTROL"))
        for key in [k for k in self.table.rows if k[0] == "TRIAL#" + run_trial.OWNER_BATCH]:
            self.table.rows.pop(key)
        with self.owner_batch(), patch.object(run_trial, "ensure_date"), self.assertRaises(SystemExit):
            self.run_silently()
        self.assertEqual(len(self.created), 2, "No call when the shared month allocation leaves no room")

    def test_hotel_batches_do_not_read_owner_secret_or_shared_budget(self):
        month = run_trial.month_budget_key()
        self.table.rows[(month["PK"], month["SK"])] = {**month, "allocatedUSD": run_trial.Decimal("19.5"), "version": 1}
        with self.ten_batch(), patch.dict(run_trial.os.environ, {"OWNER_REHEARSAL_NUMBER": self.OWNER}):
            self.run_silently()
        self.assertEqual(len(self.created), 10)
        self.assertNotIn(self.OWNER, {x["To"] for x in self.created})
        self.assertEqual(self.table.rows[(month["PK"], month["SK"])]["allocatedUSD"], run_trial.Decimal("19.5"))
        run_trial.ensure_window.assert_called_with("2026-10-16", 0)

    def test_bridge_event_types_match_recorder(self):
        import recorder
        root = Path(__file__).resolve().parents[2]
        sources = "\n".join(READ_TEXT(p) for p in (root / "cmd/phone-trial-bridge").glob("*.go") if not p.name.endswith("_test.go"))
        names = set(re.findall(r's\.emit\(c, "([a-z_]+)"', sources))
        self.assertTrue(names)
        self.assertLessEqual(names, recorder.TYPES)


if __name__ == "__main__":
    unittest.main()
