"""Collect a bounded Live Ninja log window as metadata, never raw log payloads.

Read-only. Exit 2 means incomplete coverage, not an empty/healthy window.
"""
import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import re
import subprocess
import time

PREFIXES = ("/live-ninja/", "/aws/lambda/live-ninja-", "/aws/kinesisfirehose/live-ninja-")
LABELS = set("""request_received request_timestamp request_validated request_rejected identity_failed identity_resolved operation_started operation_failed response_completed submission_started submission_completed settings_loaded queue_publish_started queue_publish_completed authority_failed authority_verified execution_received execution_returned execution_skipped execution_claimed history_loaded execution_result turn_persisted queue_batch_received queue_message_received queue_message_completed queue_batch_completed broker_received broker_authority broker_quota broker_settings broker_persona broker_guides model_started model_returned usage_recorded runner_authority runner_configured broker_invocation_started broker_invocation_completed tool_started runner_completed transcript_persisted memory_recorded validation authority messages quota settings persona guides model authorization duplicate_check session owner_lock receipt_commit queue_publish context deadline account_device settings_read settings_version receipt_read receipt_state session_read session_state expired_or_missing already_claimed pending running complete completed failed cancelled superseded expired ready answered rejected disabled account_link_required ok error success timeout stopped settings_changed execution_failed queue_unavailable worker_interrupted decode dispatch execute launch new help resume status repeat more stop chat ended missing unknown unrecognized not_configured""".split())
LABELS.update({"queue_decoded", "worker_initialization", "worker_ready"})
TOOLS = set("""call_message phone_message_status send_email set_timer set_reminder device_control get_weather set_current_location web_lookup remember_note recall_note rule_list rule_load rule_save rule_delete job_list job_status job_create job_start job_pause job_resume job_cancel job_retry job_command memory_search memory_write entity_get plan_upsert forget profile_suggest web_research knowledge_search knowledge_recent code_update_repos code_update_start code_update_status ghost_nodes ghost_sessions ghost_session_summary ghost_run_start ghost_run_status ghost_session_command deliverable_create deliverable_zip deliverable_deliver file_list file_read file_create unrecognized unknown_tool""".split())
ERROR_CODES = set("""invalid_envelope unauthorized expired busy cancelled deadline_exceeded conflict dependency_failed AccessDenied AccessDeniedException ValidationException ResourceNotFoundException ThrottlingException ProvisionedThroughputExceededException TransactionCanceledException ConditionalCheckFailedException ExpiredTokenException ServiceUnavailableException InternalServerError invalid_args unknown_tool forbidden not_found not_configured rate_limited quota_exceeded confirmation_required internal_error invalid_request unavailable timeout upstream_error alexa_unavailable alexa_access_changed alexa_context_limit alexa_prompt_limit alexa_usage_unavailable alexa_model_failed""".split())
NUMBERS = set("""elapsedMs modelMs queueWaitMs inputBytes outputBytes payloadBytes argumentBytes directiveBytes styleBytes messageCount toolCount toolCallCount round rounds tokens inputTokens outputTokens cachedTokens costUSD pollCount directiveCount settingsVersion deadlineRemainingMs batchSize failureCount receiveCount bodyBytes status statusCode durationMs billedDurationMs memorySizeMB maxMemoryUsedMB""".split())
NUMBERS.add("httpStatus")
NUMBERS.update({"activeUsers", "rolledUp"})
BOOLEANS = set("""ok applied limited keepTranscript trialMode hasResult usageReported endSession hasDevice hasSession hasLinkedToken versionSupported localeSupported skillMatched timestampValid timestampInWindow""".split())
BOOLEANS.update({"enabled", "executorReady"})
REFS = {"receipt", "receiptRef", "requestRef", "messageRef"}
FAILURE = re.compile(r"\b(error|exception|failed|failure|panic|timed out|accessdenied|throttl\w*)\b", re.I)
TYPE = re.compile(r"\*?(?:[a-z][a-z0-9_]*\.)?(?:[A-Za-z0-9_]*(?:Error|Exception)|errorString|wrapError|error)\Z")


class ReadFailure(Exception):
    pass


def instant(value):
    result = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if result.tzinfo is None:
        raise ValueError("UTC or an explicit offset is required")
    return result.astimezone(timezone.utc)


def aws_call(region, operation, params, timeout, service="logs"):
    args = ["aws", service, operation, "--region", region, "--output", "json",
            "--no-cli-pager", "--no-paginate", "--cli-connect-timeout", "10",
            "--cli-read-timeout", "30", "--cli-input-json", json.dumps(params)]
    env = dict(os.environ, AWS_MAX_ATTEMPTS="2", PYTHONUTF8="1", PYTHONIOENCODING="utf-8")
    try:
        result = subprocess.run(args, capture_output=True, timeout=timeout, env=env,
                                creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
    except subprocess.TimeoutExpired:
        raise ReadFailure("command_timeout") from None
    except OSError:
        raise ReadFailure("aws_cli_unavailable") from None
    if result.returncode:
        # Do not propagate CLI stderr: it can include URLs and credential values.
        match = re.search(rb"An error occurred \(([A-Za-z]+)\)", result.stderr)
        code = match[1].decode("ascii") if match else "aws_cli_failure"
        raise ReadFailure(code if code in ERROR_CODES else "aws_cli_failure")
    try:
        value = json.loads(result.stdout)
    except (ValueError, UnicodeError):
        raise ReadFailure("invalid_json_response") from None
    if not isinstance(value, dict):
        raise ReadFailure("invalid_response_shape")
    return value


def sanitize_event(event):
    """Allowlisted diagnostics only; unknown strings and all payloads are dropped."""
    text = event.get("message", "")
    row = {key: value for key in ("timestamp", "ingestionTime")
           if isinstance(value := event.get(key), int) and not isinstance(value, bool)}
    event_id = str(event.get("eventId", ""))
    row["eventRef"] = hashlib.sha256(event_id.encode()).hexdigest()
    try:
        record = json.loads(text)
    except (TypeError, ValueError):
        record = None
    if not isinstance(record, dict):
        row.update(kind="unstructured", candidate=bool(FAILURE.search(str(text))))
        for marker, code in {"Task timed out": "runtime_timeout", "Runtime.OutOfMemory": "out_of_memory", "Runtime.ExitError": "runtime_exit", "Alexa initialization failed": "initialization_failed"}.items():
            if marker in str(text):
                row["errorCode"] = code
                row["candidate"] = True
                break
        return row
    row["kind"] = "structured"
    level = str(record.get("level", "")).upper()
    if level in {"DEBUG", "INFO", "WARN", "WARNING", "ERROR", "FATAL"}:
        row["level"] = level
    for key in NUMBERS:
        value = record.get(key)
        if isinstance(value, (int, float)) and not isinstance(value, bool) and abs(value) < 1e18:
            row[key] = value
    for key in BOOLEANS:
        if isinstance(record.get(key), bool):
            row[key] = record[key]
    for key in ("stage", "operationStage", "reason", "outcome"):
        value = record.get(key)
        if isinstance(value, str) and value in LABELS:
            row[key] = value
    if isinstance(record.get("tool"), str) and record["tool"] in TOOLS:
        row["tool"] = record["tool"]
    for key in ("errorCode", "code"):
        if isinstance(record.get(key), str) and record[key] in ERROR_CODES:
            row["errorCode"] = record[key]
    error_type = record.get("errorType")
    if isinstance(error_type, str) and len(error_type) <= 100 and TYPE.fullmatch(error_type):
        row["errorType"] = error_type
    for key in REFS:
        value = record.get(key)
        if isinstance(value, str) and re.fullmatch("[0-9a-f]{64}", value):
            row[key] = value
    row["candidate"] = (level in {"WARN", "WARNING", "ERROR", "FATAL"}
                        or bool(record.get("errorType")) or bool(record.get("errorMessage"))
                        or bool(record.get("errorCode")) or record.get("ok") is False
                        or row.get("outcome") in {"failed", "error", "timeout"}
                        or row.get("failureCount", 0) > 0
                        or any(isinstance(record.get(k), (int, float)) and record[k] >= 400 for k in ("status", "statusCode", "httpStatus"))
                        or bool(FAILURE.search(str(record.get("msg", record.get("message", ""))))))
    return row


def collect(start, end, regions, call=aws_call, max_events=50000, max_pages=200, max_seconds=600):
    if start >= end:
        raise ValueError("start must precede end")
    if (end - start).total_seconds() > 7 * 86400:
        raise ValueError("window cannot exceed seven-day log retention")
    deadline = time.monotonic() + max_seconds
    report = {"startUTC": start.isoformat(), "endUTC": end.isoformat(), "complete": True,
              "sources": [], "sourceErrors": [], "events": 0, "candidates": 0}

    def pages(region, operation, params):
        seen, token = set(), None
        for _ in range(max_pages):
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise ReadFailure("collection_deadline")
            data = call(region, operation, {**params, **({"nextToken": token} if token else {})}, min(90, remaining))
            yield data
            token = data.get("nextToken")
            if not token:
                return
            if token in seen:
                raise ReadFailure("repeated_page_token")
            seen.add(token)
        raise ReadFailure("page_limit")

    for region in regions:
        groups = {}
        for prefix in PREFIXES:
            try:
                for page in pages(region, "describe-log-groups", {"logGroupNamePrefix": prefix}):
                    for group in page.get("logGroups", []):
                        name = group.get("logGroupName", "")
                        if name.startswith(prefix):
                            groups[name] = group
            except ReadFailure as error:
                report["complete"] = False
                report["sourceErrors"].append({"region": region, "prefix": prefix, "code": str(error)})
        for name, group in sorted(groups.items()):
            source = {"region": region, "logGroup": name, "retentionDays": group.get("retentionInDays"),
                      "complete": False, "events": [], "pages": 0}
            report["sources"].append(source)
            seen_events = set()
            try:
                # CloudWatch's endTime is inclusive; the review cursor is [start, end).
                params = {"logGroupName": name, "startTime": math.ceil(start.timestamp() * 1000),
                          "endTime": math.ceil(end.timestamp() * 1000) - 1}
                for page in pages(region, "filter-log-events", params):
                    source["pages"] += 1
                    for event in page.get("events", []):
                        event_id = event.get("eventId")
                        if event_id and event_id in seen_events:
                            continue
                        if report["events"] >= max_events:
                            raise ReadFailure("event_limit")
                        if event_id:
                            seen_events.add(event_id)
                        safe = sanitize_event(event)
                        source["events"].append(safe)
                        report["events"] += 1
                        report["candidates"] += int(safe["candidate"])
                source["complete"] = True
            except ReadFailure as error:
                report["complete"] = False
                source["errorCode"] = str(error)
            source["candidateCount"] = sum(x["candidate"] for x in source["events"])
            source["stageCounts"] = dict(Counter(x.get("stage", "unclassified") for x in source["events"]))
    report["noData"] = report["events"] == 0
    report["retentionDrift"] = [{"region": s["region"], "logGroup": s["logGroup"], "retentionDays": s["retentionDays"]}
                                for s in report["sources"] if s["retentionDays"] != 7]
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--start", required=True, type=instant)
    parser.add_argument("--end", required=True, type=instant)
    parser.add_argument("--region", action="append", default=[])
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    identity = aws_call("us-east-1", "get-caller-identity", {}, 40, service="sts")
    if identity.get("Account") != "759775734231":
        raise ReadFailure("wrong_aws_account")
    report = collect(args.start, args.end, args.region or ["us-east-1"])
    args.output.parent.mkdir(parents=True, exist_ok=True)
    temporary = args.output.with_suffix(args.output.suffix + ".tmp")
    temporary.write_text(json.dumps(report, indent=2, allow_nan=False) + "\n", encoding="utf-8")
    temporary.replace(args.output)
    print(json.dumps({k: report[k] for k in ("startUTC", "endUTC", "complete", "events", "candidates", "noData")}))
    return 0 if report["complete"] else 2


if __name__ == "__main__":
    raise SystemExit(main())
