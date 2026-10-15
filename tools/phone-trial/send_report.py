"""Send the private trial report through SES. Explicit invocation only; no import effects."""
import argparse
import datetime
import html
import json
import pathlib
import re
from email.message import EmailMessage


def build_report(report, template, targets=None):
    calls = report.get("calls", [])
    events = report.get("events", [])
    completed = sum(any(e.get("data", {}).get("type") == "call_ended" and e["data"].get("callSid") == c["callSid"]
                        and e["data"].get("data", {}).get("status") == "completed" for e in events) for c in calls)
    ready = sum(c.get("recordingStatus") == "ready" for c in calls)
    control = report.get("control") or {}
    attempted = control.get("attempted", len(calls))
    authorized = control.get("authorizedCalls", 20)
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
        entries = [e["data"] for e in events if e.get("data", {}).get("callSid") == sid]
        title = html.escape(str(call.get("targetId", sid)))
        facts = {"callSid": sid, "recordingStatus": call.get("recordingStatus"), "events": entries}
        # Evidence is the report's actual structured event data, not inferred success.
        evidence = html.escape(json.dumps(facts, indent=2, ensure_ascii=False))
        section = f'<h3 style="color:#0b0f19;background-color:#ffffff">{title}</h3><pre style="white-space:pre-wrap;background-color:#0f172a;color:#e2e8f0;padding:12px">{evidence}</pre>'
        link = call.get("recordingLink")
        if link:
            expiry = datetime.datetime.fromtimestamp(link["expiresAt"], datetime.timezone.utc).isoformat()
            section += f'<p style="color:#0b0f19;background-color:#ffffff"><a style="color:#1e3a8a;background-color:#ffffff" href="{html.escape(link["url"], quote=True)}">Listen to or download recording</a>. Access expires {html.escape(expiry)}. This private link can be used by anyone who receives it.</p>'
        else:
            section += '<p style="color:#0b0f19;background-color:#ffffff">No verified S3 recording is available for this call.</p>'
        sections.append(section)
    template = re.sub(r'  <!-- Include ONLY.*?\{\{WHAT_I_SAID_THEN_WHAT_IS_TRUE_AND_HOW_IT_WAS_CAUGHT\}\}.*?</div>\s*</div>', '', template, flags=re.S)
    lead = outcome + (". " + str(control.get("detail", "")) if control else "")
    substitutions = {"OUTCOME_ONE_LINE": lead, "SHA": report.get("runId", ""), "COMMIT_SUBJECT": "Phone trial observations; hotel claims do not verify VPN compatibility.",
                     "ACTUAL_COMMAND_OUTPUT": json.dumps({"runId": report.get("runId"), "registeredCalls": len(calls), "providerCompletedCalls": completed, "readyRecordings": ready, "control": control}, indent=2),
                     "ITEM": "Calls created or registered are not evidence that a conversation succeeded. Read each call's events below.",
                     "NEXT": "Review the hotel answers and recordings before choosing a hotel. Recordings remain private in S3; links expire seven days after first report issuance.",
                     "REPO": "JeremyProffittOrg/live-ninja", "BRANCH": "main", "HEAD_SHA": "trial report", "TIMESTAMP": datetime.datetime.now(datetime.timezone.utc).isoformat()}
    for name, value in substitutions.items():
        template = template.replace("{{" + name + "}}", html.escape(str(value)))
    body = template + '<div style="color:#0b0f19;background-color:#ffffff;font-family:Arial,sans-serif;max-width:760px;margin:auto">' + "".join(sections) + "</div>"
    if "{{" in body:
        raise ValueError("Unfilled operator template")
    return outcome, body


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--function-name", required=True)
    parser.add_argument("--run-id", default="hotel-trial-2026-10-15")
    parser.add_argument("--template", default=str(pathlib.Path.home() / ".claude/templates/email-status.html"))
    parser.add_argument("--targets", default=str(pathlib.Path(__file__).with_name("targets.json")))
    args = parser.parse_args()
    import boto3
    result = boto3.client("lambda", region_name="us-east-1").invoke(FunctionName=args.function_name,
        Payload=json.dumps({"operation": "internal_report", "runId": args.run_id}).encode())
    if result.get("FunctionError"):
        raise RuntimeError("Report function failed")
    response = json.loads(result["Payload"].read())
    if response.get("statusCode") != 200:
        raise RuntimeError("Report request rejected")
    targets_path = pathlib.Path(args.targets)
    targets = json.loads(targets_path.read_text(encoding="utf-8"))["hotels"] if targets_path.exists() else None
    outcome, body = build_report(json.loads(response["body"]), pathlib.Path(args.template).read_text(encoding="utf-8"), targets)
    message = EmailMessage()
    message["Subject"] = "live-ninja run status #1 — " + outcome
    message["From"] = "Jeremy Proffitt <jeremy@jeremy.ninja>"
    message["To"] = "proffitt.jeremy@gmail.com"
    message["Reply-To"] = "proffitt.jeremy@gmail.com"
    message.set_content(body, subtype="html", charset="utf-8")
    sent = boto3.client("ses", region_name="us-east-1").send_raw_email(Source="jeremy@jeremy.ninja", Destinations=["proffitt.jeremy@gmail.com"], RawMessage={"Data": message.as_bytes()})
    print(outcome)
    print("SES MessageId: " + sent["MessageId"])


if __name__ == "__main__":
    main()
