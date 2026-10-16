"""Send the private trial report through SES. Explicit invocation only; no import effects."""
import argparse
import datetime
import html
import json
import pathlib
import re
import time
from email.message import EmailMessage


def integer_count(value):
    """Reports can encode DynamoDB integral numbers as JSON floats."""
    if type(value) not in (int, float) or value < 0 or (isinstance(value, float) and not value.is_integer()):
        raise ValueError("Report count must be a nonnegative integer")
    return int(value)


def build_report(report, template, targets=None):
    calls = report.get("calls", [])
    events = report.get("events", [])
    completed = sum(any(e.get("data", {}).get("type") == "call_ended" and e["data"].get("callSid") == c["callSid"]
                        and e["data"].get("data", {}).get("status") == "completed" for e in events) for c in calls)
    ready = sum(c.get("recordingStatus") == "ready" for c in calls)
    control = report.get("control") or {}
    attempted = integer_count(control.get("attempted", len(calls)))
    authorized = integer_count(control.get("authorizedCalls", 20))
    outcome = f"{attempted}/{authorized} attempted; {completed} provider-completed calls; {ready} S3 recordings ready"
    if control.get("status") == "blocked":
        outcome = "Blocked: " + outcome
    consented_count = sum(call.get("consentStatus") == "granted" for call in calls)
    navigation_count = sum(call.get("recordingReason") in {"navigation_limit", "unsupported_menu", "voicemail"} for call in calls)
    live_transcripts = [event for event in events if event.get("data", {}).get("type") == "transcript"]
    interview_summaries = [event for event in events if event.get("data", {}).get("type") == "summary" and event["data"].get("data", {}).get("phase") not in {"navigation", "media"}]
    sections = []
    if control:
        detail = html.escape(str(control.get("detail", "")))
        allocation = html.escape(str(control.get("spentUSD", "unavailable")))
        label = "Blocked" if control.get("status") == "blocked" else "Outcome"
        sections.append(f'<p style="color:#0b0f19;background-color:#ffffff">{label}: {detail}</p><p style="color:#0b0f19;background-color:#ffffff">Conservative allocation: USD {allocation}. This is a budget allocation, not a measured invoice.</p>')
    sections.append(f'<p style="color:#0b0f19;background-color:#ffffff">{consented_count} calls have recorded consent; {navigation_count} ended during menu navigation or voicemail. No VPN compatibility was tested. A completed telephone call is not proof of a completed interview.</p>')
    if targets:
        attempted_targets = {call.get("targetId") for call in calls}
        remaining = [str(t.get("name", t["id"])) for t in targets if t["id"] not in attempted_targets]
        if remaining:
            sections.append('<p style="color:#0b0f19;background-color:#ffffff">Targets without a registered call: ' + html.escape("; ".join(remaining)) + '. An unknown provider outcome may still require reconciliation.</p>')
    for call in calls:
        sid = call["callSid"]
        rows = sorted([e for e in events if e.get("data", {}).get("callSid") == sid], key=lambda e: (e.get("createdAt", 0), e["data"].get("at", "")))
        entries = [e["data"] for e in rows]
        target = next((t for t in (targets or []) if t["id"] == call.get("targetId")), {})
        title = str(target.get("name", call.get("targetId", sid)))
        if target.get("city"):
            title += " — " + str(target["city"])
        section = f'<h3 style="color:#0b0f19;background-color:#ffffff">{html.escape(title)}</h3>'
        link = call.get("recordingLink")
        if link and call.get("recordingStatus") == "ready":
            expiry = datetime.datetime.fromtimestamp(link["expiresAt"], datetime.timezone.utc).isoformat()
            section += f'<p style="color:#0b0f19;background-color:#ffffff"><a style="color:#1e3a8a;background-color:#ffffff" href="{html.escape(link["url"], quote=True)}">Listen to or download recording</a>. Access expires {html.escape(expiry)}. This private link can be used by anyone who receives it.</p>'
        else:
            status = html.escape(str(call.get("recordingStatus", "unknown")))
            reason = html.escape(str(call.get("recordingReason", "No reason reported")))
            section += f'<p style="color:#0b0f19;background-color:#ffffff">No verified S3 recording is available for this call. Status: {status}; reason: {reason}.</p>'
        summaries = [e.get("data", {}) for e in entries if e.get("type") == "summary" and e.get("data", {}).get("phase") not in {"navigation", "media"}]
        summary = summaries[-1] if summaries else None
        if summary:
            finding = str(summary.get("summary", "No summary text reported"))
            if len(finding) > 700:
                finding = finding[:700] + "… [excerpt]"
            section += '<p style="color:#0b0f19;background-color:#ffffff">AI summary of staff-reported information; not VPN-tested: ' + html.escape(finding) + '</p>'
        else:
            excerpts = [str(e.get("data", {}).get("text", "")) for e in entries if e.get("type") == "transcript" and e.get("data", {}).get("speaker") == "hotel"]
            excerpt = " / ".join(excerpts)[:700]
            text = "Staff transcript excerpt; not VPN-tested: " + excerpt if excerpt else "No staff answer was captured in the live transcript. Internet login requirements remain unknown. Any offline recording transcription is in the attachment."
            section += '<p style="color:#0b0f19;background-color:#ffffff">' + html.escape(text) + '</p>'
        terminal = [e.get("data", {}).get("status") for e in entries if e.get("type") == "call_ended" and e.get("data", {}).get("status")]
        facts = {"CallSid": sid, "providerStatus": terminal[-1] if terminal else "unknown", "consent": call.get("consentStatus", "unknown"), "recordingStatus": call.get("recordingStatus", "unknown")}
        if summary:
            if len(json.dumps(summary, ensure_ascii=False)) <= 1000:
                facts["summaryEvent"] = summary
            else:
                facts["summaryEventExcerpt"] = json.dumps(summary, ensure_ascii=False)[:700]
        evidence = html.escape(json.dumps(facts, ensure_ascii=False))
        section += f'<pre style="white-space:pre-wrap;background-color:#0f172a;color:#e2e8f0;padding:10px">{evidence}</pre>'
        sections.append(section)
    template = re.sub(r'  <!-- Include ONLY.*?\{\{WHAT_I_SAID_THEN_WHAT_IS_TRUE_AND_HOW_IT_WAS_CAUGHT\}\}.*?</div>\s*</div>', '', template, flags=re.S)
    lead = outcome + (". " + str(control.get("detail", "")) if control else "")
    if ready and not live_transcripts and not interview_summaries:
        lead += ". No live AI transcript or interview summary was captured. No completed interview or VPN findings are verified. Review the recordings and attached transcription results."
    substitutions = {"OUTCOME_ONE_LINE": lead, "SHA": control.get("gitSha", "not reported"), "COMMIT_SUBJECT": "Phone trial observations; hotel claims do not verify VPN compatibility.",
                     "ACTUAL_COMMAND_OUTPUT": json.dumps({"runId": report.get("runId"), "registeredCalls": len(calls), "providerCompletedCalls": completed, "consentedCalls": consented_count, "navigationOrVoicemailExits": navigation_count, "readyRecordings": ready, "control": control}, indent=2),
                     "ITEM": "Calls created or registered are not evidence that a conversation succeeded. Compact evidence appears below; full interaction logs remain in DynamoDB. Saved consented transcript turns and offline transcription results are attached as plain text.",
                     "NEXT": "Review the hotel answers and recordings before choosing a hotel. Recordings remain private in S3; links expire seven days after first report issuance.",
                     "REPO": "JeremyProffittOrg/live-ninja", "BRANCH": "main", "HEAD_SHA": control.get("gitSha", "not reported"), "TIMESTAMP": datetime.datetime.now(datetime.timezone.utc).isoformat()}
    for name, value in substitutions.items():
        template = template.replace("{{" + name + "}}", html.escape(str(value)))
    body = template + '<div style="color:#0b0f19;background-color:#ffffff;font-family:Arial,sans-serif;max-width:760px;margin:auto">' + "".join(sections) + "</div>"
    if "{{" in body:
        raise ValueError("Unfilled operator template")
    return outcome, body


def send_once(table, ses, report, message):
    """Claim before SES. Unknown delivery never retries automatically, even after restart."""
    control = report.get("control") or {}
    if control.get("status") not in {"completed", "blocked"}:
        raise RuntimeError("A terminal trial control record is required before email")
    key = {"PK": "TRIAL#" + report["runId"], "SK": "CONTROL"}
    attempted = integer_count(control.get("attempted", 0))
    outcome_key = str(control["status"]) + ":" + str(attempted)
    if "transcriptionStatus" in control:
        outcome_key += ":transcription:" + str(control["transcriptionStatus"])
    current = table.get_item(Key=key, ConsistentRead=True).get("Item") or {}
    if current.get("mailState") == "sent" and current.get("mailOutcome") == outcome_key:
        return current["sesMessageId"], False
    if current.get("mailState") == "sending":
        raise RuntimeError("Prior SES delivery outcome is unknown; reconcile before sending again")
    claim_condition = "#status=:status AND attempted=:attempted AND (attribute_not_exists(mailState) OR (mailState=:sent AND mailOutcome<>:outcome))"
    claim_values = {":sending": "sending", ":sent": "sent", ":outcome": outcome_key, ":now": int(time.time()), ":status": control["status"], ":attempted": attempted}
    if "transcriptionStatus" in control:
        claim_condition += " AND transcriptionStatus=:transcription"
        claim_values[":transcription"] = control["transcriptionStatus"]
    table.update_item(Key=key,
        UpdateExpression="SET mailState=:sending, mailOutcome=:outcome, mailClaimedAt=:now",
        ConditionExpression=claim_condition,
        ExpressionAttributeNames={"#status": "status"},
        ExpressionAttributeValues=claim_values)
    try:
        sent = ses.send_raw_email(Source="jeremy@jeremy.ninja", Destinations=["proffitt.jeremy@gmail.com"], RawMessage={"Data": message.as_bytes()})
    except Exception:
        raise RuntimeError("SES send outcome is unknown; claim retained; do not resend without reconciliation") from None
    message_id = sent["MessageId"]
    try:
        table.update_item(Key=key, UpdateExpression="SET mailState=:sent, sesMessageId=:id, mailSentAt=:now",
            ConditionExpression="mailState=:sending AND mailOutcome=:outcome",
            ExpressionAttributeValues={":sent": "sent", ":sending": "sending", ":outcome": outcome_key, ":id": message_id, ":now": int(time.time())})
    except Exception:
        # Delivery is proven, but automatic retries must remain blocked by the claim.
        print("SES MessageId: " + message_id)
        raise RuntimeError("SES accepted email; delivery marker update failed; reconcile control row before rerun") from None
    return message_id, True


def batch_context(table, batch_id, targets):
    """Read the approved batch only; do not pull another batch's calls or links."""
    owner = batch_id == "owner-rehearsal-2026-10-16"
    if batch_id != "hotel-vpn-ten-2026-10-16" and not owner:
        raise ValueError("Unknown approved hotel batch")
    approved = {"take-%d" % n for n in range(1, 4)} if owner else {"hotel-%02d" % n for n in range(2, 12)}
    args = {"KeyConditionExpression": "PK=:pk", "ExpressionAttributeValues": {":pk": "TRIAL#" + batch_id}, "ConsistentRead": True}
    rows = []
    while True:
        page = table.query(**args)
        rows.extend(page["Items"])
        if len(rows) > 21:
            raise ValueError("Unexpected batch ledger size")
        if not page.get("LastEvaluatedKey"):
            break
        args["ExclusiveStartKey"] = page["LastEvaluatedKey"]
    control = next((row for row in rows if row["SK"] == "CONTROL"), None)
    if not control or control.get("status") not in {"completed", "blocked"} or control.get("eventRunId") != "hotel-trial-2026-10-15":
        raise ValueError("Batch must have terminal control and the expected event run")
    # DynamoDB Decimal numbers become JSON numbers before existing report helpers.
    control = json.loads(json.dumps(control, default=float))
    authorized = integer_count(control.get("authorizedCalls", 0))
    if owner:
        if not 1 <= authorized <= 3:
            raise ValueError("Owner rehearsal authorization is not one to three takes")
        approved = {"take-%d" % n for n in range(1, authorized + 1)}
    elif authorized != 10:
        raise ValueError("Batch authorization is not ten calls")
    calls = []
    for row in rows:
        if row["SK"] == "CONTROL":
            continue
        if row["SK"] not in approved or row.get("target", {}).get("id") != row["SK"]:
            raise ValueError("Unexpected batch target")
        sid = row.get("callSid")
        if sid:
            if not isinstance(sid, str) or not re.fullmatch(r"CA[0-9a-fA-F]{32}", sid) or sid in calls:
                raise ValueError("Invalid or duplicate batch CallSid")
            calls.append(sid)
    if owner:
        # Takes share the bridge target; the report names it without the owner's number.
        return control, calls, [{"id": "owner-rehearsal", "name": "Owner rehearsal (owner played the front desk)"}]
    selected = {target["id"]: target for target in (targets or []) if target["id"] in approved}
    for row in rows:
        if row["SK"] in approved:
            selected[row["SK"]] = row["target"]
    if set(selected) != approved:
        raise ValueError("Approved batch target names are incomplete")
    return control, calls, [selected[key] for key in sorted(selected)]


def transcript_attachment(report, targets=None):
    """All saved consented turns, excluding menu navigation and pre-consent speech."""
    lines = ["Live Ninja hotel call transcripts", "Batch: " + report.get("runId", "unknown"),
             "Automatic speech recognition and assistant output text; not guaranteed exact.",
             "Staff statements do not verify VPN compatibility. Pre-consent menu speech is omitted.", ""]
    for call in report.get("calls", []):
        rows = sorted([row for row in report.get("events", []) if row.get("data", {}).get("callSid") == call["callSid"]],
                      key=lambda row: (row.get("createdAt", 0), row["data"].get("at", "")))
        consented, granted, turns = False, call.get("consentStatus") == "granted", []
        for row in rows:
            event = row["data"]
            details = event.get("data", {})
            if event.get("type") == "consent":
                consented = details.get("result") == "granted"
                granted = granted or consented
            if details.get("reason") == "consent_withdrawn":
                consented = False
            if consented and event.get("type") == "transcript" and details.get("phase") not in {"navigation", "media"}:
                speaker, text = details.get("speaker"), details.get("text")
                if speaker in {"hotel", "assistant"} and isinstance(text, str):
                    timestamp = event.get("at") or str(row.get("createdAt", "time unavailable"))
                    turns.append(f"[{timestamp}] {speaker}: {text}")
        if not granted:
            continue
        target = next((target for target in (targets or []) if target["id"] == call.get("targetId")), {})
        name = target.get("name", call.get("targetId", "Unknown hotel"))
        city = target.get("city", "")
        lines.extend([str(name) + (" — " + str(city) if city else ""), "CallSid: " + call["callSid"],
                      *(turns or ["No transcript turns were saved after consent."]), ""])
        offline = report.get("offlineTranscripts", {}).get(call["callSid"])
        if offline:
            lines.extend(["Offline recording transcription: " + offline["status"],
                          "Channel speaker identities are unverified. Channel blocks are not a merged chronological conversation."])
            for channel in offline.get("channels", []):
                lines.append("Channel " + str(channel["channel"]) + " (speaker unverified; " + channel.get("status", "unknown") + "):")
                lines.append(channel.get("text") or ("No words recognized on this channel." if channel.get("status") in {"no_speech", "no_recognized_speech"} else "No transcript text is available for this channel."))
            if offline.get("note"):
                lines.append(offline["note"])
            lines.append("")
    if len(lines) == 5:
        lines.append("No calls with recorded consent events were available in this report.")
    return "\n".join(lines)


def offline_transcripts(s3, bucket, report):
    results = {}
    for call in report.get("calls", []):
        if call.get("recordingStatus") != "ready" or call.get("consentStatus") != "granted":
            continue
        sid, recording = call.get("callSid", ""), call.get("recordingSid", "")
        expected = "phone-trial/" + str(call.get("runId", "")) + "/" + sid + "/" + recording + ".mp3"
        if not re.fullmatch(r"CA[0-9a-fA-F]{32}", sid) or not re.fullmatch(r"RE[0-9a-fA-F]{32}", recording) or call.get("s3Key") != expected:
            raise ValueError("Recording metadata does not match its canonical private object")
        key = expected[:-4] + ".transcript.json"
        try:
            result = s3.get_object(Bucket=bucket, Key=key)
            stream = result["Body"]
            try:
                raw = stream.read(2 * 1024 * 1024 + 1)
            finally:
                stream.close()
            if len(raw) > 2 * 1024 * 1024:
                raise ValueError("Transcription artifact exceeds limit")
            artifact = json.loads(raw)
            if artifact.get("callSid") != sid or artifact.get("recordingSid") != recording or artifact.get("source") != "offline_asr" or artifact.get("channelAssignment") != "unverified":
                raise ValueError("Transcription identity mismatch")
            channels = artifact.get("channels", [])
            if artifact.get("status") not in {"completed", "failed", "no_recognized_speech", "no_speech"} or not isinstance(channels, list) or len(channels) > 2:
                raise ValueError("Invalid transcription result")
            seen = set()
            for channel in channels:
                if (type(channel.get("channel")) is not int or channel["channel"] not in {1, 2} or channel["channel"] in seen
                        or channel.get("speaker") != "unverified" or not isinstance(channel.get("text", ""), str)
                        or len(channel.get("text", "")) > 100000 or channel.get("status") not in {"completed", "failed", "no_recognized_speech", "no_speech"}):
                    raise ValueError("Invalid transcription channel")
                seen.add(channel["channel"])
            results[sid] = artifact
        except Exception:
            # Email the verified call outcome even when bounded ASR retrieval failed.
            results[sid] = {"status": "unavailable", "channels": [], "note": "Offline transcription is missing or could not be verified. Listen to the recording; no transcript text is inferred."}
    return results


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--function-name", required=True)
    parser.add_argument("--run-id", default="hotel-trial-2026-10-15")
    parser.add_argument("--template", default=str(pathlib.Path.home() / ".claude/templates/email-status.html"))
    parser.add_argument("--targets", default=str(pathlib.Path(__file__).with_name("targets.json")))
    parser.add_argument("--batch-id", help="Approved batch ledger to report instead of the original trial")
    parser.add_argument("--transcript-bucket", help="Private bucket containing verified offline transcription artifacts")
    args = parser.parse_args()
    import boto3
    from botocore.config import Config
    table = boto3.resource("dynamodb", region_name="us-east-1").Table("live-ninja-phone-trial")
    targets_path = pathlib.Path(args.targets)
    targets = json.loads(targets_path.read_text(encoding="utf-8"))["hotels"] if targets_path.exists() else None
    payload = {"operation": "internal_report", "runId": args.run_id}
    control, call_sids = None, None
    if args.batch_id:
        control, call_sids, targets = batch_context(table, args.batch_id, targets)
        payload.update(runId=control["eventRunId"], callSids=call_sids)
    result = boto3.client("lambda", region_name="us-east-1").invoke(FunctionName=args.function_name, Payload=json.dumps(payload).encode())
    if result.get("FunctionError"):
        raise RuntimeError("Report function failed")
    response = json.loads(result["Payload"].read())
    if response.get("statusCode") != 200:
        raise RuntimeError("Report request rejected")
    report = json.loads(response["body"])
    if args.batch_id:
        if any(call["callSid"] not in call_sids for call in report.get("calls", [])) or any(event.get("data", {}).get("callSid") not in call_sids for event in report.get("events", [])):
            raise RuntimeError("Recorder returned calls outside the approved batch; email stopped")
        report.update(runId=args.batch_id, control=control)
    if args.transcript_bucket:
        report["offlineTranscripts"] = offline_transcripts(boto3.client("s3", region_name="us-east-1"), args.transcript_bucket, report)
    outcome, body = build_report(report, pathlib.Path(args.template).read_text(encoding="utf-8"), targets)
    message = EmailMessage()
    message["Subject"] = "live-ninja run status #1 — " + outcome
    message["From"] = "Jeremy Proffitt <jeremy@jeremy.ninja>"
    message["To"] = "proffitt.jeremy@gmail.com"
    message["Reply-To"] = "proffitt.jeremy@gmail.com"
    message.set_content(body, subtype="html", charset="utf-8")
    message.add_attachment(transcript_attachment(report, targets), subtype="plain", filename="live-ninja-hotel-transcripts.txt", charset="utf-8")
    # Disable SDK retries for a non-idempotent external send.
    ses = boto3.client("ses", region_name="us-east-1", config=Config(retries={"total_max_attempts": 1}, connect_timeout=10, read_timeout=30))
    message_id, delivered_now = send_once(table, ses, report, message)
    print(outcome)
    print("SES MessageId: " + message_id)
    if not delivered_now:
        print("Same terminal outcome already emailed; duplicate skipped.")


if __name__ == "__main__":
    main()
