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
    sections = []
    if control:
        detail = html.escape(str(control.get("detail", "")))
        allocation = html.escape(str(control.get("spentUSD", "unavailable")))
        label = "Blocked" if control.get("status") == "blocked" else "Outcome"
        sections.append(f'<p style="color:#0b0f19;background-color:#ffffff">{label}: {detail}</p><p style="color:#0b0f19;background-color:#ffffff">Conservative allocation: USD {allocation}. This is a budget allocation, not a measured invoice.</p>')
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
        summaries = [e.get("data", {}) for e in entries if e.get("type") == "summary"]
        summary = summaries[-1] if summaries else None
        if summary:
            finding = str(summary.get("summary", "No summary text reported"))
            if len(finding) > 700:
                finding = finding[:700] + "… [excerpt]"
            section += '<p style="color:#0b0f19;background-color:#ffffff">AI summary of staff-reported information; not VPN-tested: ' + html.escape(finding) + '</p>'
        else:
            excerpts = [str(e.get("data", {}).get("text", "")) for e in entries if e.get("type") == "transcript" and e.get("data", {}).get("speaker") == "hotel"]
            excerpt = " / ".join(excerpts)[:700]
            text = "Staff transcript excerpt; not VPN-tested: " + excerpt if excerpt else "No staff answer was captured. Internet login requirements remain unknown."
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
    substitutions = {"OUTCOME_ONE_LINE": lead, "SHA": control.get("gitSha", "not reported"), "COMMIT_SUBJECT": "Phone trial observations; hotel claims do not verify VPN compatibility.",
                     "ACTUAL_COMMAND_OUTPUT": json.dumps({"runId": report.get("runId"), "registeredCalls": len(calls), "providerCompletedCalls": completed, "readyRecordings": ready, "control": control}, indent=2),
                     "ITEM": "Calls created or registered are not evidence that a conversation succeeded. Compact evidence appears below; full interaction logs remain in DynamoDB.",
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
    current = table.get_item(Key=key, ConsistentRead=True).get("Item") or {}
    if current.get("mailState") == "sent" and current.get("mailOutcome") == outcome_key:
        return current["sesMessageId"], False
    if current.get("mailState") == "sending":
        raise RuntimeError("Prior SES delivery outcome is unknown; reconcile before sending again")
    table.update_item(Key=key,
        UpdateExpression="SET mailState=:sending, mailOutcome=:outcome, mailClaimedAt=:now",
        ConditionExpression="#status=:status AND attempted=:attempted AND (attribute_not_exists(mailState) OR (mailState=:sent AND mailOutcome<>:outcome))",
        ExpressionAttributeNames={"#status": "status"},
        ExpressionAttributeValues={":sending": "sending", ":sent": "sent", ":outcome": outcome_key, ":now": int(time.time()), ":status": control["status"], ":attempted": attempted})
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


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--function-name", required=True)
    parser.add_argument("--run-id", default="hotel-trial-2026-10-15")
    parser.add_argument("--template", default=str(pathlib.Path.home() / ".claude/templates/email-status.html"))
    parser.add_argument("--targets", default=str(pathlib.Path(__file__).with_name("targets.json")))
    args = parser.parse_args()
    import boto3
    from botocore.config import Config
    result = boto3.client("lambda", region_name="us-east-1").invoke(FunctionName=args.function_name,
        Payload=json.dumps({"operation": "internal_report", "runId": args.run_id}).encode())
    if result.get("FunctionError"):
        raise RuntimeError("Report function failed")
    response = json.loads(result["Payload"].read())
    if response.get("statusCode") != 200:
        raise RuntimeError("Report request rejected")
    targets_path = pathlib.Path(args.targets)
    targets = json.loads(targets_path.read_text(encoding="utf-8"))["hotels"] if targets_path.exists() else None
    report = json.loads(response["body"])
    outcome, body = build_report(report, pathlib.Path(args.template).read_text(encoding="utf-8"), targets)
    message = EmailMessage()
    message["Subject"] = "live-ninja run status #1 — " + outcome
    message["From"] = "Jeremy Proffitt <jeremy@jeremy.ninja>"
    message["To"] = "proffitt.jeremy@gmail.com"
    message["Reply-To"] = "proffitt.jeremy@gmail.com"
    message.set_content(body, subtype="html", charset="utf-8")
    # Disable SDK retries for a non-idempotent external send.
    ses = boto3.client("ses", region_name="us-east-1", config=Config(retries={"total_max_attempts": 1}, connect_timeout=10, read_timeout=30))
    table = boto3.resource("dynamodb", region_name="us-east-1").Table("live-ninja-phone-trial")
    message_id, delivered_now = send_once(table, ses, report, message)
    print(outcome)
    print("SES MessageId: " + message_id)
    if not delivered_now:
        print("Same terminal outcome already emailed; duplicate skipped.")


if __name__ == "__main__":
    main()
