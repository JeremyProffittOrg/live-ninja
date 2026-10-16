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

    def test_bridge_event_types_match_recorder(self):
        import recorder
        root = Path(__file__).resolve().parents[2]
        sources = "\n".join(READ_TEXT(p) for p in (root / "cmd/phone-trial-bridge").glob("*.go") if not p.name.endswith("_test.go"))
        names = set(re.findall(r's\.emit\(c, "([a-z_]+)"', sources))
        self.assertTrue(names)
        self.assertLessEqual(names, recorder.TYPES)


if __name__ == "__main__":
    unittest.main()
