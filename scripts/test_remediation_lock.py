import json
from pathlib import Path
import tempfile
import unittest

import remediation_lock as lock
from review_cloudwatch import ReadFailure


class RemediationLockTests(unittest.TestCase):
    def test_live_run_excludes_other_tasks_and_only_its_owner_can_release(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            lock.acquire(root, "daily", "one")
            with self.assertRaisesRegex(ReadFailure, "active"):
                lock.acquire(root, "three-hour", "two", lambda _: {"observation_terminal": {"BOOL": False}})
            with self.assertRaisesRegex(ReadFailure, "another"):
                lock.release(root, "three-hour", "two")
            self.assertEqual(json.loads((root / "owner.json").read_text())["run_id"], "one")
            lock.release(root, "daily", "one")
            self.assertFalse((root / "owner.json").exists())

    def test_terminal_evidence_recovers_interrupted_run_but_unknown_does_not(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            lock.acquire(root, "daily", "one")
            with self.assertRaises(ReadFailure):
                lock.acquire(root, "three-hour", "two", lambda _: {"status": {"S": "UNKNOWN"}})
            owner = lock.acquire(root, "three-hour", "two", lambda _: {"observation_terminal": {"BOOL": True}, "status": {"S": "EXITED"}})
            self.assertEqual(owner["run_id"], "two")

    def test_observation_flag_outweighs_legacy_status(self):
        self.assertFalse(lock.terminal({"observation_terminal": {"BOOL": False}, "status": {"S": "COMPLETED"}}))
        self.assertTrue(lock.terminal({"status": {"S": "COMPLETED"}}))

    def test_os_guard_cannot_be_acquired_twice(self):
        with tempfile.TemporaryDirectory() as directory:
            with lock.guard(Path(directory)):
                with self.assertRaisesRegex(ReadFailure, "busy"):
                    with lock.guard(Path(directory)):
                        self.fail("second transaction entered")


if __name__ == "__main__":
    unittest.main()
