"""One dated, bounded trial. No redial and no credential or recording-link output."""
import base64
from datetime import datetime, timezone
from decimal import Decimal
import hashlib
import hmac
import json
import os
from pathlib import Path
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from zoneinfo import ZoneInfo

import boto3

RUN = "hotel-trial-2026-10-15"
DATE = "2026-10-15"
LIMIT = Decimal("20")
FIXED_RESERVE = Decimal("8")  # Includes registry/month, warm hosting, storage and tax margin.
CALL_RESERVE = Decimal("1.40")
MAX_SECONDS = 180
TERMINAL = {"completed", "failed", "busy", "no-answer", "canceled"}
NORMAL_END_REASONS = {"research_finished", "remote_hangup", "twilio_disconnected", "conversation_idle",
                      "duration_limit", "consent_withdrawn", "token_budget_limit",
                      "navigation_limit", "unsupported_menu", "voicemail"}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def http(url, method="GET", body=None, headers=None):
    req = urllib.request.Request(url, data=body, headers=headers or {}, method=method)
    try:
        with urllib.request.build_opener(NoRedirect).open(req, timeout=20) as response:
            return json.load(response)
    except urllib.error.HTTPError as exc:
        try:
            error = json.loads(exc.read(4096))
            provider_code = error.get("code")
        except Exception:
            provider_code = None
        suffix = " provider code " + str(provider_code) if isinstance(provider_code, int) else ""
        raise RuntimeError("HTTP " + str(exc.code) + suffix) from None
    except Exception:
        raise RuntimeError("Request failed; outcome may be unknown") from None


def twilio(path, method="GET", form=None):
    account = os.environ["TWILIO_ACCOUNT_SID"]
    auth = base64.b64encode((os.environ["TWILIO_API_KEY_SID"] + ":" + os.environ["TWILIO_API_KEY_SECRET"]).encode()).decode()
    body = urllib.parse.urlencode(form, doseq=True).encode() if form is not None else None
    return http("https://api.twilio.com/2010-04-01/Accounts/" + account + path, method, body,
                {"Authorization": "Basic " + auth, "Content-Type": "application/x-www-form-urlencoded"})


def event(sid, target, kind, **data):
    body = json.dumps({"runId": RUN, "callSid": sid, "targetId": target["id"], "type": kind,
                       "eventId": kind + "-" + target["id"], "data": data}, separators=(",", ":")).encode()
    signature = hmac.new(os.environ["TWILIO_AUTH_TOKEN"].encode(), body, hashlib.sha256).hexdigest()
    for attempt in range(3):
        try:
            return http(os.environ["RECORDER_URL"] + "/events", "POST", body,
                        {"Content-Type": "application/json", "X-LN-Signature": signature})
        except RuntimeError:
            if attempt == 2:
                raise
            time.sleep(attempt + 1)


def invoke(payload):
    result = boto3.client("lambda").invoke(FunctionName="live-ninja-phone-trial-recorder",
                                         Payload=json.dumps(payload).encode())
    if result.get("FunctionError"):
        raise RuntimeError("Recorder invocation failed")
    reply = json.load(result["Payload"])
    if reply["statusCode"] != 200:
        raise RuntimeError("Recorder operation not ready")
    return json.loads(reply["body"])


def select_caller():
    page = twilio("/IncomingPhoneNumbers.json?PageSize=1000")
    numbers = [n for n in page.get("incoming_phone_numbers", []) if n.get("capabilities", {}).get("voice")]
    if page.get("next_page_uri"):
        raise RuntimeError("Caller identity selection requires review")
    # Outbound caller ID only; do not modify any existing inbound number routing.
    named = [n for n in numbers if "ninja" in n.get("friendly_name", "").lower()]
    if len(named) == 1:
        return named[0]["phone_number"]
    if len(numbers) == 1:
        return numbers[0]["phone_number"]
    configured = os.environ.get("TWILIO_FROM_NUMBER", "")
    if configured in [n["phone_number"] for n in numbers]:
        return configured
    if not named:
        # The requested dedicated number is within the fixed infrastructure reserve.
        # Never alter routing on a number used by another application.
        available = twilio("/AvailablePhoneNumbers/US/Local.json?VoiceEnabled=true&PageSize=1").get("available_phone_numbers", [])
        if not available:
            raise RuntimeError("No available US local voice number")
        bought = twilio("/IncomingPhoneNumbers.json", "POST", {"PhoneNumber": available[0]["phone_number"],
                        "FriendlyName": "Live Ninja phone trial", "VoiceUrl": os.environ["BRIDGE_URL"] + "/unavailable"})
        return bought["phone_number"]
    raise RuntimeError("More than one Live Ninja caller ID exists; configure TWILIO_FROM_NUMBER")


def existing_month_cost():
    records = twilio("/Usage/Records/ThisMonth.json?Category=totalprice").get("usage_records", [])
    if len(records) != 1 or records[0].get("price_unit", "").lower() != "usd":
        raise RuntimeError("Monthly Twilio cost cannot be verified")
    amount = Decimal(str(records[0]["price"]))
    if not amount.is_finite() or amount < 0:
        raise RuntimeError("Invalid monthly usage cost")
    return amount


def ensure_window():
    while True:
        now = datetime.now(ZoneInfo("America/Los_Angeles"))
        if now.date().isoformat() != DATE or now.hour >= 17:
            raise RuntimeError("The authorized trial date/window has ended")
        if now.hour >= 9:
            return
        print("Waiting for the California 09:00-17:00 trial window.", flush=True)
        time.sleep(60)


def usage_charge(events, sid):
    charge = Decimal("0")
    seen = set()
    for entry in events:
        data = entry.get("data", {})
        if data.get("callSid") != sid or data.get("type") != "usage":
            continue
        if entry["SK"] in seen:
            continue
        seen.add(entry["SK"])
        usage = data.get("data", {})
        usage = usage.get("usage", usage)
        # Conservative: charge every input/output token at audio rates, no cache discount.
        charge += Decimal(str(usage.get("input_tokens", 0))) * Decimal("0.000010")
        charge += Decimal(str(usage.get("output_tokens", 0))) * Decimal("0.000020")
    return charge


def main():
    if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("EXECUTE_PHONE_TRIAL") != RUN:
        raise SystemExit("Explicit dated GitHub trial execution flag required.")
    targets = json.loads(Path("tools/phone-trial/targets.json").read_text())["hotels"]
    if len(targets) != 20 or len({t["phone"] for t in targets}) != 20:
        raise SystemExit("Exactly 20 unique destinations required.")
    table = boto3.resource("dynamodb").Table("live-ninja-phone-trial")
    ledger_key = {"PK": "TRIAL#" + RUN, "SK": "CONTROL"}
    prior = table.get_item(Key=ledger_key, ConsistentRead=True).get("Item")
    ensure_window()
    attempted_ids = set()
    reconciled_intents = set()
    if prior:
        if prior.get("status") == "completed":
            print("Trial already completed. No redial.")
            return
        if prior.get("status") == "running" or int(prior.get("executionAttempts", 1)) >= 3:
            raise SystemExit("Prior run is active/unknown or retry ceiling reached; no calls made.")
        rows = table.query(KeyConditionExpression="PK=:pk", ExpressionAttributeValues={":pk": ledger_key["PK"]}, ConsistentRead=True)
        if rows.get("LastEvaluatedKey"):
            raise SystemExit("Unexpected trial ledger pagination; no calls made.")
        spent = Decimal(str(prior["spentUSD"]))
        for row in rows["Items"]:
            if row["SK"] == "CONTROL":
                continue
            if not row.get("callSid"):
                query = urllib.parse.urlencode({"To": row["target"]["phone"], "From": prior["callerNumber"], "StartTime>=": DATE, "PageSize": 1000})
                history = twilio("/Calls.json?" + query)
                if history.get("next_page_uri") or "calls" not in history or history["calls"]:
                    raise SystemExit("Ambiguous call creation has provider history; reconcile before resuming.")
                reconciled_intents.add(row["SK"])
                continue
            if twilio("/Calls/" + row["callSid"] + ".json").get("status") not in TERMINAL:
                raise SystemExit("A prior call is not terminal; no calls made.")
            attempted_ids.add(row["SK"])
            if not row.get("settled"):
                spent += CALL_RESERVE
        table.update_item(Key=ledger_key, UpdateExpression="SET #s=:r, executionAttempts=:n, spentUSD=:spent",
                          ConditionExpression="#s=:old", ExpressionAttributeNames={"#s": "status"},
                          ExpressionAttributeValues={":r": "running", ":old": prior["status"], ":n": int(prior.get("executionAttempts", 1)) + 1, ":spent": spent})
    else:
        spent = FIXED_RESERVE + existing_month_cost()
        table.put_item(Item={**ledger_key, "status": "running", "authorizedCalls": 20, "startedAt": int(time.time()), "gitSha": os.environ.get("GITHUB_SHA", "unknown"),
                            "executionAttempts": 1, "reservedInfrastructureUSD": FIXED_RESERVE, "spentUSD": spent},
                       ConditionExpression="attribute_not_exists(PK)")
    attempts = len(attempted_ids)
    failure = None
    sid = None
    details = {}
    try:
        if spent + CALL_RESERVE > LIMIT:
            raise RuntimeError("Monthly allowance requires explicit owner permission")
        caller = select_caller()
        table.update_item(Key=ledger_key, UpdateExpression="SET callerNumber=:n", ExpressionAttributeValues={":n": caller})
        for target in targets:
            if target["id"] in attempted_ids:
                continue
            ensure_window()
            if spent + CALL_RESERVE > LIMIT:
                failure = "Monthly $20 threshold reached; explicit $5 daily permission required."
                break
            control = table.get_item(Key=ledger_key, ConsistentRead=True)["Item"]
            if control.get("status") != "running":
                failure = "Trial stopped by owner."
                break
            url = os.environ["BRIDGE_URL"] + "/voice?" + urllib.parse.urlencode({"run": RUN, "target": target["id"]})
            attempt_key = {"PK": ledger_key["PK"], "SK": target["id"]}
            if target["id"] in reconciled_intents:
                table.update_item(Key=attempt_key, UpdateExpression="SET lastAttemptAt=:now, noPriorCallVerifiedAt=:now",
                                  ExpressionAttributeValues={":now": int(time.time())})
            else:
                table.put_item(Item={**attempt_key, "target": target, "createdAt": int(time.time()),
                                    "reserveUSD": CALL_RESERVE, "settled": False}, ConditionExpression="attribute_not_exists(PK)")
            # A failed/ambiguous create is never retried. No recording until voice consent.
            created = twilio("/Calls.json", "POST", {"To": target["phone"], "From": caller, "Url": url,
                             "Method": "POST", "Timeout": 20, "TimeLimit": MAX_SECONDS})
            sid = created["sid"]
            details = created
            attempts += 1
            table.update_item(Key=attempt_key, UpdateExpression="SET callSid=:sid", ExpressionAttributeValues={":sid": sid})
            event(sid, target, "call_registered", hotel=target["name"], city=target["city"], source=target["source"])
            print("Call attempt " + str(attempts) + "/20: " + target["id"], flush=True)
            deadline = time.monotonic() + MAX_SECONDS + 45
            while details.get("status") not in TERMINAL and time.monotonic() < deadline:
                time.sleep(5)
                details = twilio("/Calls/" + sid + ".json")
                if table.get_item(Key=ledger_key, ConsistentRead=True)["Item"].get("status") != "running":
                    details = twilio("/Calls/" + sid + ".json", "POST", {"Status": "completed"})
                    break
            if details.get("status") not in TERMINAL:
                twilio("/Calls/" + sid + ".json", "POST", {"Status": "completed"})
                details = twilio("/Calls/" + sid + ".json")
            if details.get("status") not in TERMINAL:
                raise RuntimeError("Provider call end is not confirmed; remaining calls stopped")
            event(sid, target, "call_ended", status=details.get("status"), duration=details.get("duration"),
                  providerPrice=details.get("price"), providerCurrency=details.get("price_unit"))
            if details.get("status") == "failed":
                raise RuntimeError("Provider reported a failed call; remaining destinations held for diagnosis")
            copied = False
            for retry in range(3):
                recordings = twilio("/Calls/" + sid + "/Recordings.json").get("recordings", [])
                if not recordings:
                    time.sleep(5)
                    continue
                copied = True
                for rec in recordings:
                    if rec.get("status") == "completed":
                        try:
                            invoke({"operation": "internal_reconcile", "runId": RUN, "callSid": sid, "recordingSid": rec["sid"]})
                        except RuntimeError:
                            copied = False
                            if retry == 2:
                                raise
                    else:
                        copied = False
                if copied:
                    break
                time.sleep(30 if retry == 0 else 60)
            if recordings and not copied:
                raise RuntimeError("Recording has not reached verified S3 storage")
            report = invoke({"operation": "internal_report", "runId": RUN, "includeLinks": False})
            duration = Decimal(str(details.get("duration") or 0))
            # Round telephone/stream/recording billing up to the next whole minute.
            minutes = (duration / 60).to_integral_value(rounding="ROUND_CEILING")
            related = [e.get("data", {}) for e in report["events"] if e.get("data", {}).get("callSid") == sid]
            finals = [e.get("data", {}) for e in related if e.get("type") == "call_ended" and e.get("data", {}).get("usageComplete") is True]
            measured = usage_charge(report["events"], sid)
            if finals:
                final = max(finals, key=lambda f: int(f.get("inputTokens", 0)) + int(f.get("outputTokens", 0)))
                measured = max(measured, Decimal(str(final.get("inputTokens", 0))) * Decimal("0.000010") + Decimal(str(final.get("outputTokens", 0))) * Decimal("0.000020"))
            cost = measured + minutes * Decimal("0.0209") + Decimal("0.21")
            consented = any(e.get("type") == "consent" and e.get("data", {}).get("result") == "granted" for e in related)
            # Incomplete usage remains reserved, never silently charged as zero.
            if consented and not finals:
                cost = max(cost, CALL_RESERVE)
            spent += cost
            table.update_item(Key=ledger_key, UpdateExpression="SET spentUSD=:s, attempted=:a", ExpressionAttributeValues={":s": spent, ":a": attempts})
            table.update_item(Key=attempt_key, UpdateExpression="SET settled=:yes, costUSD=:cost", ExpressionAttributeValues={":yes": True, ":cost": cost})
            print("Attempt complete; conservative month allocation USD " + str(spent.quantize(Decimal("0.01"))), flush=True)
            technical = [e for e in related if e.get("type") == "error" or
                         (e.get("type") == "call_ended" and e.get("data", {}).get("reason") and
                          e["data"]["reason"] not in NORMAL_END_REASONS)]
            if technical:
                failure = "Technical error in canary call; remaining calls held for repair."
                break
    except Exception as exc:
        failure = "Trial interrupted: " + (str(exc) if isinstance(exc, RuntimeError) else type(exc).__name__) + ". Reconcile before retry; do not redial."
        if sid and details.get("status") not in TERMINAL:
            try:
                twilio("/Calls/" + sid + ".json", "POST", {"Status": "completed"})
            except Exception:
                failure += " Provider stop unconfirmed; provider-side 180-second limit remains in force."
    table.update_item(Key=ledger_key, UpdateExpression="SET #s=:s, attempted=:a, detail=:d, endedAt=:e",
                      ExpressionAttributeNames={"#s": "status"}, ExpressionAttributeValues={":s": "blocked" if failure else "completed", ":a": attempts,
                      ":d": failure or "All 20 attempts reached a terminal call state.", ":e": int(time.time())})
    print("Trial terminal: " + str(attempts) + "/20 attempted. " + (failure or "Completed."), flush=True)
    if failure:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
