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
OWNER_BATCH = "owner-rehearsal-2026-10-16"
OWNER_DATE = "2026-10-16"
OWNER_BRIDGE_TARGET = "owner-rehearsal"
OWNER_TAKE_PAUSE = 45
TEN_BATCH = "hotel-vpn-ten-2026-10-16"
NORMAL_END_REASONS = {"research_finished", "remote_hangup", "twilio_disconnected", "conversation_idle",
                      "duration_limit", "consent_withdrawn", "token_budget_limit",
                      "navigation_limit", "unsupported_menu", "voicemail", "consent_declined"}


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


def bridge_target(target):
    """The bridge knows each telephone number once; owner takes share one bridge target."""
    return target.get("bridgeId", target["id"])


def event(sid, target, kind, **data):
    body = json.dumps({"runId": RUN, "callSid": sid, "targetId": bridge_target(target), "type": kind,
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
    if not named and os.environ.get("PHONE_TRIAL_BATCH", RUN) == RUN:
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


def ensure_window(date=DATE, start_hour=9):
    while True:
        now = datetime.now(ZoneInfo("America/Los_Angeles"))
        if now.date().isoformat() != date or now.hour >= 17:
            raise RuntimeError("The authorized trial date/window has ended")
        if now.hour >= start_hour:
            return
        print("Waiting for the California 09:00-17:00 trial window.", flush=True)
        time.sleep(60)


def ensure_date(date):
    """Owner rehearsal: the owner's own phone, so no hotel hours; still one dated authorization."""
    if datetime.now(ZoneInfo("America/Los_Angeles")).date().isoformat() != date:
        raise RuntimeError("The authorized owner rehearsal date has ended")


def owner_targets():
    """Only the owner allowlisted number from the OWNER_REHEARSAL_NUMBER secret; never a file or input."""
    number = os.environ.get("OWNER_REHEARSAL_NUMBER", "").strip()
    if len(number) != 12 or not number.startswith("+1") or not number[2:].isdigit():
        raise SystemExit("OWNER_REHEARSAL_NUMBER secret is missing or not an E.164 US number; no calls made.")
    takes = os.environ.get("OWNER_REHEARSAL_TAKES", "3")
    if takes not in {"1", "2", "3"}:
        raise SystemExit("OWNER_REHEARSAL_TAKES must be 1, 2 or 3; no calls made.")
    return [{"id": "take-%d" % n, "bridgeId": OWNER_BRIDGE_TARGET, "name": "Owner rehearsal take %d" % n,
             "phone": number, "city": "", "source": "owner-rehearsal"} for n in range(1, int(takes) + 1)]


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


def batch_profile():
    batch = os.environ.get("PHONE_TRIAL_BATCH", RUN)
    if batch == RUN:
        return batch, DATE, 9, None
    if batch == TEN_BATCH:
        return batch, "2026-10-16", 0, ["hotel-%02d" % n for n in range(2, 12)]
    if batch == OWNER_BATCH:
        return batch, OWNER_DATE, None, None
    raise SystemExit("Unknown dated batch; no calls authorized.")


def ledger_floor(table, ledger):
    rows = table.query(KeyConditionExpression="PK=:pk", ExpressionAttributeValues={":pk": "TRIAL#" + ledger}, ConsistentRead=True)
    if rows.get("LastEvaluatedKey"):
        raise RuntimeError("Prior budget ledger pagination requires reconciliation")
    control = next((r for r in rows["Items"] if r["SK"] == "CONTROL"), {})
    floor = Decimal(str(control.get("spentUSD", FIXED_RESERVE)))
    for row in rows["Items"]:
        if row["SK"] != "CONTROL" and not row.get("settled"):
            floor += max(CALL_RESERVE, Decimal(str(row.get("reserveUSD", CALL_RESERVE))))
    return floor


def month_budget_key():
    return {"PK": "PHONE#BUDGET#" + datetime.now(timezone.utc).strftime("%Y-%m"), "SK": "MONTH"}


def prior_budget_floor(table, batch=None):
    floor = max(FIXED_RESERVE + existing_month_cost(), ledger_floor(table, RUN))
    if batch == OWNER_BATCH:
        # Every earlier allocation this month counts: both hotel ledgers and owner phone messages.
        floor = max(floor, ledger_floor(table, TEN_BATCH))
        month = table.get_item(Key=month_budget_key(), ConsistentRead=True).get("Item", {})
        floor = max(floor, Decimal(str(month.get("allocatedUSD", "0"))))
    return floor


def share_month_allocation(table, amount, replaces=None):
    """Owner rehearsal publishes its allocation to the shared monthly budget used by owner messages.

    It only raises the shared allocation, except that a settled take may replace exactly the
    reservation this run wrote while it still holds the voice lock."""
    key = month_budget_key()
    old = table.get_item(Key=key, ConsistentRead=True).get("Item")
    current = Decimal(str((old or {}).get("allocatedUSD", "0")))
    if amount == current or (amount < current and current != replaces):
        return
    version = int((old or {}).get("version", 0))
    if old:
        table.update_item(Key=key, UpdateExpression="SET allocatedUSD=:a, version=:v", ConditionExpression="version=:old",
                          ExpressionAttributeValues={":a": amount, ":v": version + 1, ":old": version})
    else:
        table.put_item(Item={**key, "allocatedUSD": amount, "version": 1}, ConditionExpression="attribute_not_exists(PK)")


def main():
    batch, date, start_hour, target_ids = batch_profile()
    if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("EXECUTE_PHONE_TRIAL") != batch:
        raise SystemExit("Explicit dated GitHub trial execution flag required.")
    table = boto3.resource("dynamodb").Table("live-ninja-phone-trial")
    lock_key = {"PK": "PHONE#CONTROL", "SK": "VOICE"}
    owner = "batch:" + batch
    current = table.get_item(Key=lock_key, ConsistentRead=True).get("Item", {})
    state = {"safe": False}
    if current.get("activeRequest") != owner:
        try:
            table.update_item(Key=lock_key, UpdateExpression="SET activeRequest=:owner",
                              ConditionExpression="attribute_not_exists(activeRequest)",
                              ExpressionAttributeValues={":owner": owner})
        except Exception:
            raise SystemExit("Another call owner holds the voice lock or lock access failed; no calls made.") from None
        state["safe"] = True
    try:
        execute_batch(table, batch, date, start_hour, target_ids, state)
    finally:
        if state["safe"]:
            try:
                table.update_item(Key=lock_key, UpdateExpression="REMOVE activeRequest",
                                  ConditionExpression="activeRequest=:owner", ExpressionAttributeValues={":owner": owner})
            except Exception:
                raise SystemExit("Voice lock release could not be confirmed; reconcile before another call.") from None


def execute_batch(table, batch, date, start_hour, target_ids, state):
    if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("EXECUTE_PHONE_TRIAL") != batch:
        raise SystemExit("Explicit dated GitHub trial execution flag required.")
    owner = batch == OWNER_BATCH
    if owner:
        targets = owner_targets()
    else:
        targets = json.loads(Path("tools/phone-trial/targets.json").read_text())["hotels"]
        if len(targets) != 20 or len({t["phone"] for t in targets}) != 20:
            raise SystemExit("Exactly 20 unique destinations required.")
    if target_ids is not None:
        by_id = {t["id"]: t for t in targets}
        if len(by_id) != len(targets) or any(t not in by_id for t in target_ids):
            raise SystemExit("Approved destination list is incomplete or duplicated.")
        targets = [by_id[t] for t in target_ids]
    count = len(targets)
    ledger_key = {"PK": "TRIAL#" + batch, "SK": "CONTROL"}
    prior = table.get_item(Key=ledger_key, ConsistentRead=True).get("Item")

    def check_window():
        if owner:
            ensure_date(date)
        else:
            ensure_window(date, start_hour)
    check_window()
    attempted_ids = set()
    reconciled_intents = set()
    if prior:
        state["safe"] = False
        if prior.get("status") == "completed":
            state["safe"] = True
            print("Trial already completed. No redial.")
            return
        if prior.get("status") == "running" or int(prior.get("executionAttempts", 1)) >= 3:
            state["safe"] = False
            raise SystemExit("Prior run is active/unknown or retry ceiling reached; no calls made.")
        rows = table.query(KeyConditionExpression="PK=:pk", ExpressionAttributeValues={":pk": ledger_key["PK"]}, ConsistentRead=True)
        if rows.get("LastEvaluatedKey"):
            raise SystemExit("Unexpected trial ledger pagination; no calls made.")
        spent = Decimal(str(prior["spentUSD"]))
        for row in rows["Items"]:
            if row["SK"] == "CONTROL":
                continue
            if not row.get("callSid"):
                query = urllib.parse.urlencode({"To": row["target"]["phone"], "From": prior["callerNumber"], "StartTime>=": date, "PageSize": 1000})
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
        spent = prior_budget_floor(table, batch) if batch != RUN else FIXED_RESERVE + existing_month_cost()
        table.put_item(Item={**ledger_key, "status": "running", "authorizedCalls": count, "eventRunId": RUN, "authorizedDate": date, "startedAt": int(time.time()), "gitSha": os.environ.get("GITHUB_SHA", "unknown"),
                            "executionAttempts": 1, "reservedInfrastructureUSD": FIXED_RESERVE, "spentUSD": spent},
                       ConditionExpression="attribute_not_exists(PK)")
    state["safe"] = True
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
            if owner and attempts > len(attempted_ids):
                # Strictly sequential takes: the previous take is terminal and settled; let the owner reset.
                print("Pausing " + str(OWNER_TAKE_PAUSE) + " seconds before the next take.", flush=True)
                time.sleep(OWNER_TAKE_PAUSE)
            check_window()
            if spent + CALL_RESERVE > LIMIT:
                failure = "Monthly $20 threshold reached; explicit $5 daily permission required."
                break
            control = table.get_item(Key=ledger_key, ConsistentRead=True)["Item"]
            if control.get("status") != "running":
                failure = "Trial stopped by owner."
                break
            if owner and (target["phone"] != os.environ.get("OWNER_REHEARSAL_NUMBER", "").strip() or bridge_target(target) != OWNER_BRIDGE_TARGET):
                raise RuntimeError("Owner rehearsal refuses any destination other than the owner secret")
            url = os.environ["BRIDGE_URL"] + "/voice?" + urllib.parse.urlencode({"run": RUN, "target": bridge_target(target)})
            attempt_key = {"PK": ledger_key["PK"], "SK": target["id"]}
            if target["id"] in reconciled_intents:
                table.update_item(Key=attempt_key, UpdateExpression="SET lastAttemptAt=:now, noPriorCallVerifiedAt=:now",
                                  ExpressionAttributeValues={":now": int(time.time())})
            else:
                table.put_item(Item={**attempt_key, "target": target, "createdAt": int(time.time()),
                                    "reserveUSD": CALL_RESERVE, "settled": False}, ConditionExpression="attribute_not_exists(PK)")
            if owner:
                reserved = spent + CALL_RESERVE
                share_month_allocation(table, reserved)
            # A failed/ambiguous create is never retried. No recording until voice consent.
            state["safe"] = False
            sid = None
            created = twilio("/Calls.json", "POST", {"To": target["phone"], "From": caller, "Url": url,
                             "Method": "POST", "Timeout": 20, "TimeLimit": MAX_SECONDS})
            sid = created["sid"]
            details = created
            attempts += 1
            table.update_item(Key=attempt_key, UpdateExpression="SET callSid=:sid", ExpressionAttributeValues={":sid": sid})
            event(sid, target, "call_registered", hotel=target["name"], city=target["city"], source=target["source"])
            print("Call attempt " + str(attempts) + "/" + str(count) + ": " + target["id"], flush=True)
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
            state["safe"] = True
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
            cost = measured + minutes * Decimal("0.0209") + Decimal("0.25")
            consented = any(e.get("type") == "consent" and e.get("data", {}).get("result") == "granted" for e in related)
            # Incomplete usage remains reserved, never silently charged as zero.
            if consented and not finals:
                cost = max(cost, CALL_RESERVE)
            spent += cost
            table.update_item(Key=ledger_key, UpdateExpression="SET spentUSD=:s, attempted=:a", ExpressionAttributeValues={":s": spent, ":a": attempts})
            table.update_item(Key=attempt_key, UpdateExpression="SET settled=:yes, costUSD=:cost", ExpressionAttributeValues={":yes": True, ":cost": cost})
            if owner:
                share_month_allocation(table, spent, replaces=reserved)
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
                state["safe"] = twilio("/Calls/" + sid + ".json").get("status") in TERMINAL
            except Exception:
                failure += " Provider stop unconfirmed; provider-side 180-second limit remains in force."
    table.update_item(Key=ledger_key, UpdateExpression="SET #s=:s, attempted=:a, detail=:d, endedAt=:e",
                      ExpressionAttributeNames={"#s": "status"}, ExpressionAttributeValues={":s": "blocked" if failure else "completed", ":a": attempts,
                      ":d": failure or "All " + str(count) + " attempts reached a terminal call state.", ":e": int(time.time())})
    print("Trial terminal: " + str(attempts) + "/" + str(count) + " attempted. " + (failure or "Completed."), flush=True)
    if failure:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
