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
        row = self.rows[(Key["PK"], Key["SK"])]
        names = ExpressionAttributeNames or {}
        for assignment in UpdateExpression.removeprefix("SET ").split(","):
            key, value = [v.strip() for v in assignment.split("=")]
            row[names.get(key, key)] = ExpressionAttributeValues[value]

    def query(self, **kwargs):
        return {"Items": list(self.rows.values())}


class RunnerTests(unittest.TestCase):
    def setUp(self):
        self.table = Table()
        self.created = []
        self.month = run_trial.Decimal("2")
        self.fail_create = False
        self.targets = [{"id": "hotel-" + str(i), "name": "Synthetic Hotel", "phone": "+1555000" + str(1000 + i), "city": "Test", "source": "https://example.invalid"} for i in range(20)]
        self.patches = [
            patch.dict(run_trial.os.environ, {"GITHUB_ACTIONS": "true", "EXECUTE_PHONE_TRIAL": run_trial.RUN, "BRIDGE_URL": "https://example.invalid"}),
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
            return {"sid": "CA" + str(len(self.created)).zfill(32), "status": "completed", "duration": "10"}
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

    def test_ambiguous_creation_blocks_automatic_retry(self):
        self.fail_create = True
        with self.assertRaises(SystemExit):
            self.run_silently()
        self.fail_create = False
        with self.assertRaises(SystemExit):
            self.run_silently()
        self.assertEqual(self.created, [])

    def test_missing_execution_flag_has_no_side_effect(self):
        with patch.dict(run_trial.os.environ, {"EXECUTE_PHONE_TRIAL": ""}):
            with self.assertRaises(SystemExit):
                self.run_silently()
        self.assertEqual(self.table.rows, {})

    def test_usage_duplicate_events_not_double_charged(self):
        entry = {"SK": "event", "data": {"callSid": "CA1", "type": "usage", "data": {"input_tokens": 1000, "output_tokens": 500}}}
        self.assertEqual(run_trial.usage_charge([entry, entry], "CA1"), run_trial.Decimal("0.020"))

    def test_bridge_event_types_match_recorder(self):
        import recorder
        root = Path(__file__).resolve().parents[2]
        sources = "\n".join(READ_TEXT(p) for p in (root / "cmd/phone-trial-bridge").glob("*.go") if not p.name.endswith("_test.go"))
        names = set(re.findall(r's\.emit\(c, "([a-z_]+)"', sources))
        self.assertTrue(names)
        self.assertLessEqual(names, recorder.TYPES)


if __name__ == "__main__":
    unittest.main()
