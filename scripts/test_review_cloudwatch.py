import json
import unittest
from datetime import datetime, timedelta, timezone

import review_cloudwatch as review


class CloudWatchReviewTests(unittest.TestCase):
    def setUp(self):
        self.start = datetime(2026, 10, 24, tzinfo=timezone.utc)
        self.end = self.start + timedelta(hours=3)

    def test_private_payloads_and_unknown_strings_never_leave_collector(self):
        event = {"eventId": "event", "timestamp": 1, "message": json.dumps({
            "level": "ERROR", "stage": "identity_failed", "errorCode": "unauthorized",
            "msg": "PRIVATE-MESSAGE Bearer token", "accessToken": "PRIVATE-TOKEN",
            "deviceId": "PRIVATE-DEVICE", "text": "PRIVATE-TRANSCRIPT",
            "arguments": "PRIVATE-ARGS", "output": "PRIVATE-OUTPUT",
            "reason": "PRIVATE-REASON", "tool": "PRIVATE-TOOL", "elapsedMs": 43,
            "errorType": "PRIVATE-TYPE Bearer token", "requestRef": "a" * 64,
        })}
        safe = review.sanitize_event(event)
        self.assertTrue(safe["candidate"])
        self.assertEqual(safe["stage"], "identity_failed")
        self.assertEqual(safe["errorCode"], "unauthorized")
        self.assertEqual(safe["elapsedMs"], 43)
        self.assertNotIn("PRIVATE", json.dumps(safe))
        self.assertNotIn("Bearer", json.dumps(safe))
        self.assertNotIn("accessToken", safe)

    def test_unstructured_failure_is_detected_without_exporting_its_text(self):
        safe = review.sanitize_event({"eventId": "event", "message": "Task timed out; Authorization: PRIVATE-TOKEN"})
        self.assertTrue(safe["candidate"])
        self.assertEqual(safe["kind"], "unstructured")
        self.assertNotIn("PRIVATE", json.dumps(safe))

    def test_lambda_error_without_a_level_is_still_a_candidate(self):
        safe = review.sanitize_event({"eventId": "runtime", "message": json.dumps({
            "errorType": "Runtime.ExitError", "errorMessage": "PRIVATE-ERROR-BODY",
        })})
        self.assertTrue(safe["candidate"])
        self.assertNotIn("PRIVATE", json.dumps(safe))

    def test_non_scalar_labels_and_unknown_timestamps_are_dropped(self):
        safe = review.sanitize_event({"eventId": "test", "timestamp": "PRIVATE-TIME", "message": json.dumps({
            "stage": {"PRIVATE": "VALUE"}, "outcome": ["PRIVATE"], "tool": ["PRIVATE"],
            "status": {}, "errorCode": {"PRIVATE": "VALUE"}, "failureCount": 2,
        })})
        self.assertTrue(safe["candidate"])
        self.assertNotIn("PRIVATE", json.dumps(safe))

    def test_empty_pages_with_tokens_are_fully_read_and_duplicate_events_deduplicated(self):
        calls = []
        def api(region, operation, params, timeout):
            calls.append((operation, params.copy()))
            if operation == "describe-log-groups":
                if params["logGroupNamePrefix"] != review.PREFIXES[0]:
                    return {}
                if "nextToken" not in params:
                    return {"logGroups": [], "nextToken": "groups-next"}
                return {"logGroups": [{"logGroupName": "/live-ninja/lambda/alexa-skill", "retentionInDays": 7}]}
            self.assertEqual(params["startTime"], int(self.start.timestamp() * 1000))
            self.assertEqual(params["endTime"], int(self.end.timestamp() * 1000) - 1)
            if "nextToken" not in params:
                return {"events": [], "nextToken": "events-next"}
            event = {"eventId": "unique", "message": '{"level":"INFO","stage":"request_received"}'}
            return {"events": [event, event]}
        result = review.collect(self.start, self.end, ["us-east-1"], call=api)
        self.assertTrue(result["complete"])
        self.assertEqual(result["events"], 1)
        self.assertEqual(result["candidates"], 0)
        self.assertEqual(result["sources"][0]["pages"], 2)
        self.assertEqual(result["retentionDrift"], [])
        self.assertTrue(any(p.get("nextToken") == "groups-next" for _, p in calls))

    def test_failed_source_is_partial_not_empty_success(self):
        def api(region, operation, params, timeout):
            if operation == "describe-log-groups":
                return {"logGroups": [{"logGroupName": params["logGroupNamePrefix"] + "test"}]}
            raise review.ReadFailure("AccessDeniedException")
        result = review.collect(self.start, self.end, ["us-east-1"], call=api)
        self.assertFalse(result["complete"])
        self.assertTrue(result["noData"])
        self.assertTrue(all(s["errorCode"] == "AccessDeniedException" for s in result["sources"]))
        self.assertEqual(len(result["retentionDrift"]), len(review.PREFIXES))

    def test_repeated_tokens_and_event_ceiling_mark_incomplete_coverage(self):
        def api(region, operation, params, timeout):
            if operation == "describe-log-groups":
                return {"logGroups": [{"logGroupName": params["logGroupNamePrefix"] + "test", "retentionInDays": 7}]}
            return {"events": [{"eventId": str(len(params)), "message": "ERROR PRIVATE"}], "nextToken": "same"}
        result = review.collect(self.start, self.end, ["us-east-1"], call=api)
        self.assertFalse(result["complete"])
        self.assertTrue(all(s["errorCode"] == "repeated_page_token" for s in result["sources"]))
        result = review.collect(self.start, self.end, ["us-east-1"], call=api, max_events=1)
        self.assertFalse(result["complete"])
        self.assertEqual(result["events"], 1)
        self.assertNotIn("PRIVATE", json.dumps(result))

    def test_bad_window_is_rejected_before_any_read(self):
        with self.assertRaises(ValueError):
            review.collect(self.end, self.start, [])
        with self.assertRaises(ValueError):
            review.collect(self.start, self.start + timedelta(days=8), [])
        with self.assertRaises(ValueError):
            review.instant("2026-10-24T12:00:00")


if __name__ == "__main__":
    unittest.main()
