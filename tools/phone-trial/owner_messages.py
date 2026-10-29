"""IAM-only, owner-bound messages to privately configured phone aliases."""
import base64
from datetime import datetime, timezone, timedelta
from zoneinfo import ZoneInfo
from decimal import Decimal, InvalidOperation
import hashlib
import hmac
import html
import json
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

import recorder as r

CONFIG_KEY = {"PK": "CONFIG", "SK": "OWNER_MESSAGES"}
VOICE_KEY = {"PK": "PHONE#CONTROL", "SK": "VOICE"}
RESERVE = Decimal("0.30")
# Owner decision 2026-10-19: "cap per call to $10 in cost and a 10 minute
# limit." Every owner conversation (incoming, or outgoing live) is admitted
# with a model cost cap and a 600 second deadline. The cap handed to the bridge
# is min($10, remaining day allowance, remaining month allowance) minus the
# telephone reserve for a full-length call; the whole amount is reserved until
# the call settles at its measured cost.
PER_CALL_CAP = Decimal("10")
MAX_CALL_SECONDS = 600
# Telephone rates already used by the trial ledger (run_trial.py): voice minute
# with media stream and dual-channel recording, billed per started minute, plus
# a fixed recording charge for every connected call.
TELEPHONE_MINUTE_USD = Decimal("0.0209")
TELEPHONE_CALL_USD = Decimal("0.25")
# Owner decision 2026-10-19: "okay, let's set a daily call allowance of $10
# with $100 monthly cap". A standing allowance: outgoing calls may reserve up
# to $10 per owner-local day with no grant, and every call this month,
# incoming and outgoing, shares the $100 cap. The separate incoming $10/day
# policy is unchanged. The month total keeps the fixed $8 infrastructure floor.
DAILY_ALLOWANCE = Decimal("10")
MONTHLY_CAP = Decimal("100")
INFRA_FLOOR = Decimal("8")
# Ledger version 2 records each reservation under "unsettled" and replaces it
# with the measured cost when the call settles. Version 1 months only summed
# reservations and over-counted.
LEDGER_VERSION = 2
TERMINAL = {"completed", "busy", "failed", "no-answer", "canceled"}
STATUSES = TERMINAL | {"queued", "initiated", "ringing", "in-progress"}
INTRO = "This is Live Ninja, Jeremy's AI assistant. This owner message is being recorded. "
PHONE = re.compile(r"^\+[1-9][0-9]{7,14}$")
REQUEST_ID = re.compile(r"^[A-Za-z0-9_-]{1,100}$")


class ProviderError(Exception):
    def __init__(self, code=None, rejected=False):
        self.code, self.rejected = code, rejected


class AllowanceRefused(ValueError):
    """Standing call allowance refusal; reason is daily_allowance_used or monthly_cap_reached."""

    def __init__(self, reason, message):
        super().__init__(reason + ": " + message)
        self.reason = reason


class InboundDailyLimit(AllowanceRefused):
    """The separately authorized incoming daily allowance cannot admit a call."""

    def __init__(self, message="Incoming daily allowance is used for today"):
        super().__init__("daily_allowance_used", message)


def telephone_estimate(seconds):
    """Telephone cost of a connected call of this length, at the ledger rates."""
    seconds = int(seconds)
    if seconds <= 0:
        return Decimal("0")
    minutes = (Decimal(seconds) / 60).to_integral_value(rounding="ROUND_CEILING")
    return minutes * TELEPHONE_MINUTE_USD + TELEPHONE_CALL_USD


# Telephone reserve for a full 600 second owner conversation: $0.459.
TELEPHONE_RESERVE = telephone_estimate(MAX_CALL_SECONDS)


def cost_cap_string(value):
    """Wire form of costCapUSD / modelCostUSD: a decimal string with six places."""
    return str(Decimal(str(value)).quantize(Decimal("0.000001")))


def parse_model_cost(value):
    """modelCostUSD from the bridge finish: a decimal string with six places in
    [0, PER_CALL_CAP], or None when invalid. One call can never cost the model
    more than the per-call cap, so a larger figure is a bridge defect, not a cost."""
    if not isinstance(value, str) or not re.fullmatch(r"[0-9]{1,4}\.[0-9]{6}", value):
        return None
    cost = Decimal(value)
    if not cost.is_finite() or cost < 0 or cost > PER_CALL_CAP:
        return None
    return cost


def monthly_cap_refusal():
    return AllowanceRefused("monthly_cap_reached", "This month's $100 call cap is reached; no call created")


def daily_allowance_refusal():
    return AllowanceRefused("daily_allowance_used", "Today's $10 outgoing call allowance is used; no call created")


def provider(path, form=None):
    """One attempt only, including POST. Never expose provider response text."""
    account = os.environ["TWILIO_ACCOUNT_SID"]
    auth = base64.b64encode((os.environ.get("TWILIO_API_KEY_SID") or account).encode() + b":" +
                           (os.environ.get("TWILIO_API_KEY_SECRET") or os.environ["TWILIO_AUTH_TOKEN"]).encode()).decode()
    url = f"https://api.twilio.com/2010-04-01/Accounts/{account}{path}"
    data = urllib.parse.urlencode(form, doseq=True).encode() if form is not None else None
    req = urllib.request.Request(url, data=data, headers={"Authorization": "Basic " + auth, "Content-Type": "application/x-www-form-urlencoded"})
    try:
        with urllib.request.build_opener(r.NoRedirect).open(req, timeout=10) as source:
            raw = source.read(256 * 1024 + 1)
            if len(raw) > 256 * 1024:
                raise ValueError("Response too large")
            return json.loads(raw, parse_float=Decimal)
    except urllib.error.HTTPError as exc:
        code = None
        try:
            code = json.loads(exc.read(4096)).get("code")
        except Exception:
            pass
        code = code if type(code) is int else None
        raise ProviderError(code, exc.code in {400, 401, 403, 404, 422} and code is not None) from None
    except Exception:
        raise ProviderError() from None


def config(owner, sending=False):
    try:
        if str(uuid.UUID(owner)) != owner:
            return None
    except (ValueError, TypeError, AttributeError):
        return None
    import account_lifecycle
    if r.get(account_lifecycle.owner_key(owner)):
        return None
    value = r.get(CONFIG_KEY)
    if not value or value.get("ownerUserId") != owner or (sending and value.get("enabled") is not True):
        return None
    return value


def request_key(owner, request_id):
    return {"PK": "OWNER#" + owner, "SK": "MESSAGE#" + request_id}


def namespace(owner):
    return "owner-messages#" + owner


def recording_run(item):
    source = item.get('recordingRunId') or namespace(item['ownerUserId'])
    if source != namespace(item['ownerUserId']) and (source != os.environ['RUN_ID'] or not item.get('phoneResearchJobId') or not item.get('phoneResearchRunId')):
        raise ValueError('Invalid recording namespace')
    return source


def transact(operations):
    r.clients()[0].meta.client.transact_write_items(TransactItems=operations)


def put(item, old=None):
    value = {"TableName": os.environ["TABLE"], "Item": item,
             "ConditionExpression": "#v=:v" if old else "attribute_not_exists(PK)"}
    if old:
        value.update(ExpressionAttributeNames={"#v": "version"}, ExpressionAttributeValues={":v": old["version"]})
    return {"Put": value}


def change(key, fields, event_type, event_data, sid=None, release=False):
    """Append callback evidence and update state/binding atomically."""
    event_hash = hashlib.sha256(json.dumps([event_type, event_data], sort_keys=True).encode()).hexdigest()
    event_key = {"PK": key["PK"], "SK": "EVENT#" + key["SK"][8:] + "#" + event_hash}
    for _ in range(4):
        old = r.get(key)
        if not old:
            raise ValueError("Unknown message")
        import account_lifecycle
        run = recording_run(old)
        bound = {"ownerUserId": old["ownerUserId"], "runId": run}
        if account_lifecycle.blocked(bound):
            return old
        if sid and old.get("callSid", sid) != sid:
            raise ValueError("Call binding mismatch")
        if r.get(event_key):
            return old
        new = {**old, **fields, "version": int(old["version"]) + 1, "updatedAt": int(time.time())}
        # Delayed create responses/status callbacks must not move a call backwards.
        rank = {"creating": 0, "unknown": 0, "queued": 1, "initiated": 2, "ringing": 3, "in-progress": 4}
        old_status, new_status = old.get("providerStatus"), fields.get("providerStatus")
        if old_status in TERMINAL or rank.get(old_status, 0) > rank.get(new_status, 5 if new_status in TERMINAL else 0):
            new["providerStatus"] = old_status
        if sid:
            new["callSid"] = sid
        operations = [put(new, old), put({**event_key, "type": event_type, "data": event_data, "createdAt": int(time.time())})]
        operations.extend(account_lifecycle.ready_conditions(bound))
        if sid:
            call_key = r.call_key(sid, run)
            call = r.get(call_key)
            if call and call.get("requestId") not in {None, old["requestId"]}:
                raise ValueError("Call already bound")
            research = {name: old[name] for name in ('phoneResearchJobId', 'phoneResearchRunId', 'phoneResearchGeneration') if name in old}
            if call and not call.get('requestId'):
                if not research or call.get('targetId') != old['destination'] or call.get('runId') != run or call.get('ownerUserId', old['ownerUserId']) != old['ownerUserId']:
                    raise ValueError('Call owner binding mismatch')
                operations.append(put({**call, **research, 'ownerUserId': old['ownerUserId'], 'requestId': old['requestId'], 'version': int(call['version']) + 1}, call))
            if not call:
                operations.append(put({**call_key, **research, "version": 1, "runId": run, "callSid": sid,
                    "ownerUserId": old["ownerUserId"], "requestId": old["requestId"], "targetId": old["destination"],
                    "direction": old.get("direction", "outbound"), "recordingStatus": "pending",
                    "recordingCreateState": new.get("recordingCreateState", "not_requested")}))
        lock = r.get(VOICE_KEY) if release else None
        if lock and lock.get("activeRequest") == old["lockId"]:
            operations.append({"Update": {"TableName": os.environ["TABLE"], "Key": VOICE_KEY,
                "UpdateExpression": "REMOVE activeRequest", "ConditionExpression": "activeRequest=:request",
                "ExpressionAttributeValues": {":request": old["lockId"]}}})
        try:
            transact(operations)
            return new
        except Exception as exc:
            if not r.conditional(exc):
                raise
    raise RuntimeError("Concurrent message update; retry status lookup")


def view(item, links=False):
    payload = {"requestId": item["requestId"], "destination": item["destination"], "accepted": bool(item.get("callSid")),
               "providerStatus": item["providerStatus"], "playbackCompleted": item.get("playbackCompleted", False),
               "humanHeard": "not_verified", "scriptTranscript": item["scriptTranscript"], "transcriptType": "spoken_script_not_speech_recognition",
               "recordingStatus": "pending" if item.get("callSid") else "absent", "reservedUSD": str(item.get('reservedUSD', RESERVE)), "live": item.get('live', False)}
    if item.get('live') is True:
        payload['transcriptType'] = 'delivery_summary_not_speech_recognition'
        payload['scriptTranscript'] = item['message']
    if item.get('costCapUSD') is not None:
        payload['costCapUSD'] = cost_cap_string(item['costCapUSD'])
        payload['maxCallSeconds'] = int(item.get('maxCallSeconds', MAX_CALL_SECONDS))
    if item.get("providerCode"):
        payload["providerCode"] = item["providerCode"]
    if item.get("callSid"):
        payload["callSid"] = item["callSid"]
        call = r.get(r.call_key(item["callSid"], namespace(item["ownerUserId"])))
        if call:
            payload["recordingStatus"] = call["recordingStatus"]
            if links and call["recordingStatus"] == "ready":
                payload["recordingLink"] = r.issue_link(call)
    return payload


def budget_clock(cfg):
    name = cfg.get("timezone") or "America/New_York"
    zone = ZoneInfo(name)
    now = datetime.now(zone)
    tomorrow = datetime.combine(now.date() + timedelta(days=1), datetime.min.time(), tzinfo=zone)
    return now.strftime("%Y-%m"), now.strftime("%Y-%m-%d"), name, int(tomorrow.timestamp())


def budget_keys(month, day):
    return ({"PK": "PHONE#BUDGET#" + month, "SK": "MONTH"},
            {"PK": "PHONE#BUDGET#" + month, "SK": "DAY#" + day})


def inbound_budget_key(owner, day):
    return {'PK': 'OWNER#' + owner, 'SK': 'INBOUND_BUDGET#' + day}


def inbound_daily_policy(cfg, inbound, caller=None):
    policy = (inbound or {}).get('dailyBudget')
    if (not isinstance(policy, dict) or policy.get('enabled') is not True
            or (caller is not None and policy.get('callerNumber') != caller)):
        return None
    try:
        limit = Decimal(str(policy.get('limitUSD')))
        version = Decimal(str(policy.get('version')))
    except (InvalidOperation, ValueError, TypeError):
        raise ValueError('Incoming daily allowance configuration is invalid') from None
    if (inbound.get('ownerUserId') != cfg.get('ownerUserId') or not isinstance(policy.get('callerNumber'), str)
            or not PHONE.fullmatch(policy['callerNumber']) or policy['callerNumber'] not in cfg.get('destinations', {}).values()
            or not limit.is_finite() or limit != Decimal('10') or not version.is_finite()
            or version < 1 or version != version.to_integral_value()):
        raise ValueError('Incoming daily allowance configuration is invalid')
    return policy


def inbound_budget_summary(cfg, day, zone, expires):
    result = {'enabled': False, 'limitUSD': '0', 'reservedUSD': '0', 'remainingUSD': '0',
              'day': day, 'timezone': zone, 'expiresAt': expires}
    inbound = r.get({'PK': 'CONFIG', 'SK': 'INBOUND_OWNER'})
    try:
        policy = inbound_daily_policy(cfg, inbound)
        if policy is None:
            return result
        prior = r.get(inbound_budget_key(cfg['ownerUserId'], day)) or {}
        reserved = Decimal(str(prior.get('reservedUSD', '0')))
        if (not reserved.is_finite() or reserved < 0 or reserved > Decimal('10')
                or (prior and (prior.get('ownerUserId') != cfg['ownerUserId'] or prior.get('day') != day or prior.get('timezone') != zone))):
            return result
        return {**result, 'enabled': True, 'limitUSD': '10', 'reservedUSD': str(reserved),
                'remainingUSD': str(Decimal('10') - reserved)}
    except (ValueError, InvalidOperation, TypeError):
        return result


def floor_for(cfg, month):
    floor = Decimal(str(cfg.get("budgetFloorUSD", INFRA_FLOOR)))
    floor_month = cfg.get("budgetFloorMonth")
    if not floor.is_finite() or floor < INFRA_FLOOR:
        raise ValueError("Invalid monthly budget")
    if floor_month is not None and (not isinstance(floor_month, str) or not re.fullmatch(r"[0-9]{4}-(0[1-9]|1[0-2])", floor_month)):
        raise ValueError("Invalid budget floor month")
    if floor > INFRA_FLOOR and floor_month is None:
        raise ValueError("A budget floor above $8 requires budgetFloorMonth")
    return floor if floor_month == month else INFRA_FLOOR


def observed_provider_usd():
    usage = provider("/Usage/Records/ThisMonth.json?Category=totalprice")
    records = usage.get("usage_records", [])
    if len(records) != 1 or records[0].get("price_unit", "").lower() != "usd" or usage.get("next_page_uri"):
        raise ValueError("Monthly provider cost unavailable")
    observed = Decimal(str(records[0].get("price")))
    if not observed.is_finite() or observed < 0:
        raise ValueError("Invalid monthly budget")
    return observed


def month_allocated(month_row, floor):
    """Month total at or above the floor. Admission requires a version 2 row;
    a version 1 row is only displayed until rebase_month replaces it."""
    return max(floor, Decimal(str((month_row or {}).get("allocatedUSD", "0"))))


def rebase_month(cfg, month, floor, observed=None):
    """Replace a version 1 month total with the $8 floor plus measured provider cost.

    Runs only while no call holds the shared voice lock, so no reservation is
    in flight. Any later reservation is tracked and settled individually.
    """
    month_key, _ = budget_keys(month, "")
    for _ in range(3):
        old = r.get(month_key)
        if (old or {}).get("ledgerVersion") == LEDGER_VERSION:
            return old
        if (r.get(VOICE_KEY) or {}).get("activeRequest"):
            return old
        if observed is None:
            observed = observed_provider_usd()
        new = {**(old or {}), **month_key, "allocatedUSD": max(floor, INFRA_FLOOR + observed), "ledgerVersion": LEDGER_VERSION,
               "unsettled": {}, "observedProviderUSD": observed, "observedAt": int(time.time()),
               "rebasedFromUSD": Decimal(str((old or {}).get("allocatedUSD", "0"))), "rebasedAt": int(time.time()),
               "version": int((old or {}).get("version", 0)) + 1}
        try:
            transact([put(new, old)])
            return new
        except Exception as exc:
            if not r.conditional(exc):
                raise
    return r.get(month_key)


def measured_cost(item):
    """Measured cost of a terminal call, or None when it is not known yet."""
    status = item.get("providerStatus")
    if status == "rejected" and not item.get("callSid"):
        return Decimal("0")
    sid = item.get("callSid", "")
    if not r.SID.fullmatch(sid) or (r.get(VOICE_KEY) or {}).get("activeRequest") == item.get("lockId"):
        return None
    if status not in TERMINAL and item.get("inboundState") != "ended":
        return None
    try:
        call = provider("/Calls/" + sid + ".json")
    except ProviderError:
        return None
    if (call.get("sid") != sid or call.get("account_sid") != os.environ["TWILIO_ACCOUNT_SID"]
            or call.get("status") not in TERMINAL or str(call.get("price_unit", "")).upper() != "USD"
            or call.get("price") in (None, "")):
        return None
    try:
        cost = abs(Decimal(str(call["price"])))
    except (InvalidOperation, ValueError):
        return None
    if not cost.is_finite():
        return None
    if item.get("direction") == "inbound" or item.get("live") is True or item.get("bridge"):
        # Owner conversation: telephone estimate for the actual duration (never
        # below the provider's own measured price) plus the model cost the bridge
        # priced from every response.done usage object. An unknown model cost
        # keeps the reservation (owner decision 2026-10-19: never below measured).
        duration = int(call.get("duration") or 0)
        cost = max(cost, telephone_estimate(duration))
        if call.get("status") == "completed" and duration > 0:
            model = item.get("modelCostUSD")
            if model is None and "redeemedAt" not in item:
                # The bridge never redeemed the bootstrap (provider cap failure,
                # bootstrap failure, connect expiry, revocation, or a hangup
                # during the announcement): no model turn ever ran, so the
                # model cost is a known zero rather than an unknown.
                model = Decimal(0)
            if type(model) not in (int, Decimal) or model < 0 or not Decimal(model).is_finite():
                return None
            cost += Decimal(model)
    return cost


def settle(cfg, limit=10):
    """True up terminal calls: replace each reservation with its measured cost.

    The month and day totals move by the same difference. A call whose cost is
    still unknown keeps its reservation and is retried by a later read.
    """
    month, _, _, _ = budget_clock(cfg)
    owner = cfg["ownerUserId"]
    month_key, _ = budget_keys(month, "")
    settled = 0
    row = r.get(month_key) or {}
    for request_id in sorted((row.get("unsettled") or {}))[:limit]:
        for _ in range(3):
            month_row = r.get(month_key) or {}
            reserved = (month_row.get("unsettled") or {}).get(request_id)
            if month_row.get("ledgerVersion") != LEDGER_VERSION or reserved is None:
                break
            item = r.get(request_key(owner, request_id))
            if not item or item.get("budgetMonth") != month:
                break
            cost = measured_cost(item)
            if cost is None:
                break
            delta = cost - Decimal(str(reserved))
            unsettled = {k: v for k, v in month_row["unsettled"].items() if k != request_id}
            month_new = {**month_row, "allocatedUSD": Decimal(str(month_row.get("allocatedUSD", "0"))) + delta,
                         "unsettled": unsettled, "version": int(month_row["version"]) + 1}
            item_new = {**item, "settledUSD": cost, "settledAt": int(time.time()), "version": int(item["version"]) + 1}
            operations = [put(month_new, month_row), put(item_new, item)]
            day_key = {"PK": item.get("budgetDayPK", ""), "SK": item.get("budgetDaySK", "")}
            day_row = r.get(day_key) if day_key["PK"] and day_key["SK"] else None
            if day_row and "reservedUSD" in day_row:
                day_reserved = max(Decimal(0), Decimal(str(day_row["reservedUSD"])) + delta)
                if item.get("budgetSource") == "incoming-daily-policy":
                    day_reserved = min(DAILY_ALLOWANCE, day_reserved)
                operations.append(put({**day_row, "reservedUSD": day_reserved, "version": int(day_row["version"]) + 1}, day_row))
            try:
                transact(operations)
                settled += 1
                break
            except Exception as exc:
                if not r.conditional(exc):
                    raise
    return settled


def budget_status(cfg):
    month, day, zone, expires = budget_clock(cfg)
    floor = floor_for(cfg, month)
    month_key, day_key = budget_keys(month, day)
    try:
        rebase_month(cfg, month, floor)
        settle(cfg)
    except (ProviderError, ValueError):
        # Status still reports the stored ledger; unknown costs keep reservations.
        pass
    monthly, daily = r.get(month_key) or {}, r.get(day_key) or {}
    allocated = month_allocated(monthly, floor)
    reserved = Decimal(str(daily.get("reservedUSD", "0")))
    lock = r.get(VOICE_KEY) or {}
    stopped = bool(lock.get("activeRequest"))
    month_left = max(Decimal(0), MONTHLY_CAP - allocated)
    day_left = max(Decimal(0), DAILY_ALLOWANCE - reserved)
    # pausedReason reports whether an owner conversation can be admitted: reserve()
    # refuses one when the remaining day or month allowance is at or below the
    # telephone reserve (the model cost cap would be zero). A prerecorded message
    # needs only RESERVE, so it may still be sent in the narrow band between the two.
    reason = ("disabled" if cfg.get("enabled") is not True else "voice_stop_or_active_call" if stopped
              else "monthly_cap_reached" if month_left <= TELEPHONE_RESERVE
              else "daily_allowance_used" if day_left <= TELEPHONE_RESERVE else "")
    return {"month": month, "day": day, "timezone": zone, "allocatedUSD": str(allocated),
            "monthlyCapUSD": str(MONTHLY_CAP), "monthlyRemainingUSD": str(month_left),
            "dailyAllowanceUSD": str(DAILY_ALLOWANCE), "dailyReservedUSD": str(reserved), "dailyRemainingUSD": str(day_left),
            "unsettledCalls": len((monthly.get("unsettled") or {}) if monthly.get("ledgerVersion") == LEDGER_VERSION else {}),
            "requiredReservationUSD": str(PER_CALL_CAP), "minimumReservationUSD": str(RESERVE), "expiresAt": expires,
            "perCallCapUSD": str(PER_CALL_CAP), "perCallMaxSeconds": MAX_CALL_SECONDS, "telephoneReserveUSD": str(TELEPHONE_RESERVE),
            "pausedReason": reason, "pendingReservation": stopped,
            "incomingDailyBudget": inbound_budget_summary(cfg, day, zone, expires)}


def budget_operation(event):
    if event.get("operation") not in {"owner_budget_status", "owner_budget_grant"}:
        return r.response(400, {"error": "Unknown budget operation"})
    cfg = config(event.get("ownerUserId"))
    if not cfg:
        return r.response(403, {"error": "Owner budget is not configured for this identity"})
    if event["operation"] == "owner_budget_grant":
        # Retired 2026-10-19: the standing daily allowance needs no approval.
        return r.response(410, {"error": "Daily approvals are retired. Outgoing calls use a standing $10 daily allowance within the $100 monthly cap."})
    return r.response(200, budget_status(cfg))


def reserve(owner, request_id, alias, message, cfg, *, reserve_amount=RESERVE, initial_fields=None, extra_checks=(), target_number=None, inbound_policy=None, telephone_reserve=None):
    """Admit one call against the ledger.

    With telephone_reserve set (owner conversations), the reservation is
    min(PER_CALL_CAP, remaining day allowance, remaining month allowance); the
    part above the telephone reserve is the model cost cap handed to the bridge
    as costCapUSD. A cap at or below zero refuses the call. Otherwise the fixed
    reserve_amount is reserved (prerecorded messages).
    """
    if telephone_reserve is not None and (not telephone_reserve.is_finite() or telephone_reserve <= 0 or telephone_reserve >= PER_CALL_CAP):
        raise ValueError("Invalid telephone reserve")
    observed = observed_provider_usd()
    month, day, zone, expires = budget_clock(cfg)
    floor = floor_for(cfg, month)
    budget_key, day_key = budget_keys(month, day)
    if not r.get(request_key(owner, request_id)):
        rebase_month(cfg, month, floor, observed)
        settle(cfg, limit=3)
    key = request_key(owner, request_id)
    callback_id = r.digest(("owner-message\n" + owner + "\n" + request_id).encode())
    lock_id = "owner:" + owner + ":" + request_id
    item = {**key, "ownerUserId": owner, "requestId": request_id, "destination": alias, "message": message,
            "scriptTranscript": INTRO + message, "providerStatus": "creating", "recordingCreateState": "not_requested", "version": 1, "createdAt": int(time.time()),
            "lockId": lock_id, "callbackId": callback_id, "fromNumber": cfg["fromNumber"], "toNumber": target_number if target_number is not None else cfg["destinations"][alias]}
    if not PHONE.fullmatch(item['toNumber']):
        raise ValueError('Invalid approved destination')
    item.update(initial_fields or {})
    if inbound_policy is not None:
        if (item.get('direction') != 'inbound' or not r.SID.fullmatch(item.get('callSid', ''))
                or request_id != 'inbound-' + item['callSid'] or target_number is not None or telephone_reserve is None
                or inbound_daily_policy(cfg, {'ownerUserId': owner, 'dailyBudget': inbound_policy}, item['toNumber']) is None):
            raise ValueError('Recurring incoming allowance requires an exact inbound call')
    for _ in range(4):
        prior = r.get(key)
        if prior:
            return prior, False
        old = r.get(budget_key)
        if (old or {}).get("ledgerVersion") != LEDGER_VERSION:
            raise ValueError("Monthly ledger is being rebased; retry call admission")
        base = max(floor, INFRA_FLOOR + observed, month_allocated(old, floor))
        if budget_clock(cfg)[1] != day:
            raise ValueError("Day changed; retry call admission")
        month_left = MONTHLY_CAP - base
        if telephone_reserve is None and base + reserve_amount > MONTHLY_CAP:
            raise monthly_cap_refusal()
        if telephone_reserve is not None and month_left - telephone_reserve <= 0:
            raise monthly_cap_refusal()
        daily_key = inbound_budget_key(owner, day) if inbound_policy is not None else day_key
        daily_old = r.get(daily_key)
        daily = None
        reserved = Decimal(str((daily_old or {}).get("reservedUSD", "0")))
        if inbound_policy is not None:
            if (not reserved.is_finite() or reserved < 0 or reserved > Decimal('10')
                    or (daily_old and (daily_old.get('ownerUserId') != owner or daily_old.get('day') != day or daily_old.get('timezone') != zone))):
                raise ValueError('Incoming daily allowance ledger is invalid')
            day_limit = Decimal('10')
        else:
            if not reserved.is_finite() or reserved < 0:
                raise ValueError("Daily call allowance ledger is invalid")
            day_limit = DAILY_ALLOWANCE
        if telephone_reserve is not None:
            amount = min(PER_CALL_CAP, day_limit - reserved, month_left)
            cost_cap = amount - telephone_reserve
            if cost_cap <= 0:
                raise InboundDailyLimit() if inbound_policy is not None else daily_allowance_refusal()
            item.update(costCapUSD=cost_cap, telephoneReserveUSD=telephone_reserve, maxCallSeconds=MAX_CALL_SECONDS)
        else:
            amount = reserve_amount
            if reserved + amount > day_limit:
                raise InboundDailyLimit() if inbound_policy is not None else daily_allowance_refusal()
        if inbound_policy is not None:
            daily = {**(daily_old or {}), **daily_key, 'ownerUserId': owner, 'day': day, 'timezone': zone,
                     'limitUSD': Decimal('10'), 'reservedUSD': reserved + amount, 'expiresAt': expires,
                     'version': int((daily_old or {}).get('version', 0)) + 1}
            item.update(budgetSource='incoming-daily-policy', inboundBudgetDay=day)
        else:
            daily = {**(daily_old or {}), **daily_key, "day": day, "timezone": zone, "limitUSD": DAILY_ALLOWANCE,
                     "reservedUSD": reserved + amount, "expiresAt": expires,
                     "version": int((daily_old or {}).get('version', 0)) + 1}
            item.update(budgetSource="standing-daily-allowance")
        item.update(reservedUSD=amount, budgetMonth=month, budgetDayPK=daily_key["PK"], budgetDaySK=daily_key["SK"])
        unsettled = {**(old.get("unsettled") or {}), request_id: amount}
        budget = {**old, **budget_key, "allocatedUSD": base + amount, "unsettled": unsettled,
                  "observedProviderUSD": observed, "observedAt": int(time.time()),
                  "version": int(old.get("version", 0)) + 1}
        config_check = {"TableName": os.environ["TABLE"], "Key": CONFIG_KEY,
            "ConditionExpression": "#enabled=:enabled AND #owner=:owner AND #from=:from AND #destinations=:destinations",
            "ExpressionAttributeNames": {"#enabled": "enabled", "#owner": "ownerUserId", "#from": "fromNumber", "#destinations": "destinations"},
            "ExpressionAttributeValues": {":enabled": True, ":owner": owner, ":from": cfg["fromNumber"], ":destinations": cfg["destinations"]}}
        # A concurrent floor/month change must invalidate this budget reservation.
        for name in ("budgetFloorUSD", "budgetFloorMonth", "timezone"):
            config_check["ExpressionAttributeNames"]["#" + name] = name
            if name in cfg:
                config_check["ConditionExpression"] += " AND #" + name + "=:" + name
                config_check["ExpressionAttributeValues"][":" + name] = cfg[name]
            else:
                config_check["ConditionExpression"] += " AND attribute_not_exists(#" + name + ")"
        operations = [put(item), put(budget, old),
            {"Put": {"TableName": os.environ["TABLE"], "Item": {**VOICE_KEY, "activeRequest": lock_id}, "ConditionExpression": "attribute_not_exists(activeRequest)"}},
            put({"PK": "OWNER_CALLBACK#" + callback_id, "SK": "META", "ownerUserId": owner, "requestId": request_id}),
            {"ConditionCheck": config_check}]
        import account_lifecycle
        operations.extend(account_lifecycle.ready_conditions({"ownerUserId": owner, "runId": namespace(owner)}))
        operations.extend(extra_checks)
        if inbound_policy is not None:
            # The incoming allowance stays bounded to its own local day.
            item["budgetExpiresAt"] = int(daily["expiresAt"])
        operations.append(put(daily, daily_old))
        if budget_clock(cfg)[1] != day or int(time.time()) >= expires:
            raise ValueError('Day changed before call reservation; retry admission')
        try:
            transact(operations)
            return item, True
        except Exception as exc:
            if not r.conditional(exc):
                raise
            lock = r.get(VOICE_KEY)
            if lock and lock.get("activeRequest"):
                existing = r.get(key)
                if existing:
                    return existing, False
                raise ValueError("Another voice call or unresolved call holds the shared lock")
    raise ValueError("Budget or configuration changed; no call created")


def operation(event):
    owner, request_id = event.get("ownerUserId"), event.get("requestId", "")
    sending = event["operation"] == "owner_message_send"
    cfg = config(owner, sending)
    if not cfg:
        return r.response(403, {"error": "Owner phone messages are not configured for this identity"})
    if not isinstance(request_id, str) or not REQUEST_ID.fullmatch(request_id):
        return r.response(400, {"error": "Invalid requestId"})
    key = request_key(owner, request_id)
    prior = r.get(key)
    if not sending:
        return r.response(200, view(prior, links=True)) if prior else r.response(404, {"error": "Unknown message request"})
    alias, message = event.get("destination"), event.get("message")
    live, payload = event.get('live', False), event.get('payload', '')
    if type(live) is not bool or not isinstance(payload, str) or len(payload.encode('utf-8')) > 12000:
        return r.response(400, {'error': 'Invalid live delivery payload'})
    if payload:
        try:
            parsed = json.loads(payload, parse_constant=lambda value: (_ for _ in ()).throw(ValueError('Invalid JSON number')))
            if not live or not isinstance(parsed, dict):
                raise ValueError('Object required')
        except (ValueError, RecursionError):
            return r.response(400, {'error': 'Payload must be a JSON object and requires live=true'})
    if alias not in {"primary", "secondary"} or not isinstance(message, str) or not 1 <= len(message.strip()) <= 1000 or len(message) > 1000 or any(ord(c) < 32 and c not in "\n\t" for c in message):
        return r.response(400, {"error": "Use a configured alias and 1–1000 characters of plain text"})
    if prior:
        if prior["destination"] != alias or prior["message"] != message or prior.get('live', False) != live or prior.get('payload', '') != payload:
            return r.response(409, {"error": "requestId already identifies a different message"})
        return r.response(200, view(prior))
    number = cfg.get("destinations", {}).get(alias, "")
    if not PHONE.fullmatch(number) or not PHONE.fullmatch(cfg.get("fromNumber", "")):
        return r.response(403, {"error": "Destination is not configured"})
    try:
        fields = {}
        if live:
            import inbound_owner
            bridge = os.environ.get('INBOUND_BRIDGE_URL', '').rstrip('/')
            parsed = urllib.parse.urlsplit(bridge)
            if parsed.scheme != 'https' or not parsed.hostname or parsed.path or parsed.query or parsed.fragment or parsed.username or not inbound_owner.valid_config(cfg, r.get(inbound_owner.CONFIG), since=int(time.time())):
                return r.response(403, {'error': 'Live owner conversations are unavailable'})
            fields = {'live': True, 'payload': payload, 'direction': 'outbound', 'inboundState': 'connecting',
                      'deadline': int(time.time()) + MAX_CALL_SECONDS, 'connectDeadline': int(time.time()) + 35,
                      'bridge': bridge}
        item, fresh = reserve(owner, request_id, alias, message, cfg, reserve_amount=RESERVE,
                              telephone_reserve=TELEPHONE_RESERVE if live else None, initial_fields=fields)
    except ValueError as exc:
        return r.response(409, {"error": str(exc), **({"reason": exc.reason} if isinstance(exc, AllowanceRefused) else {})})
    except ProviderError:
        return r.response(409, {"error": "Provider usage cannot be verified; no call created"})
    if not fresh:
        if item["destination"] != alias or item["message"] != message or item.get('live', False) != live or item.get('payload', '') != payload:
            return r.response(409, {"error": "requestId already identifies a different message"})
        return r.response(200, view(item))
    if live:
        item = inbound_owner.transition(item, deadline=min(item['deadline'], int(item.get('budgetExpiresAt', item['deadline']))))
    base = os.environ["PUBLIC_BASE_URL"].rstrip("/") + "/owner-message/"
    token = item["callbackId"]
    twiml = '<Response><Say>' + html.escape(item["scriptTranscript"]) + '</Say><Redirect method="POST">' + html.escape(base + "played/" + token) + '</Redirect></Response>'
    if live:
        twiml = inbound_owner.connect_redirect(item)['body']
    # Reserve ten seconds for the create request and twenty seconds for ringing.
    # A live conversation may run MAX_CALL_SECONDS; a prerecorded message keeps its 90 s cap.
    call_seconds = min(MAX_CALL_SECONDS if live else 90, int(item.get("budgetExpiresAt", int(time.time())+((MAX_CALL_SECONDS + 30) if live else 120)))-int(time.time())-30)
    if call_seconds <= 0:
        item = change(key, {"providerStatus": "rejected"}, "budget_expired", {}, release=True)
        return r.response(409, {"error": "Daily authority expires before a call can connect"})
    try:
        item = change(key, {"recordingCreateState": "attempted"}, "recording_create_attempted", {})
        import account_lifecycle
        if item.get("recordingCreateState") != "attempted" or account_lifecycle.blocked({"ownerUserId": owner, "runId": namespace(owner)}):
            if item.get("recordingCreateState") == "attempted" and not item.get("callSid"):
                # The fresh deletion check ran before POST, so this attempt is
                # definitively aborted. Preserve that fact even though normal
                # message writes are fenced; never recreate a removed request.
                abort = {"Update": {"TableName": os.environ["TABLE"], "Key": key,
                    "UpdateExpression": "SET providerStatus=:rejected, recordingCreateState=:rejected, #v=:next",
                    "ConditionExpression": "#v=:v AND ownerUserId=:owner AND recordingCreateState=:attempted AND attribute_not_exists(callSid)",
                    "ExpressionAttributeNames": {"#v": "version"},
                    "ExpressionAttributeValues": {":v": item["version"], ":next": int(item["version"])+1,
                        ":owner": owner, ":attempted": "attempted", ":rejected": "rejected"}}}
                checks = [{"ConditionCheck": {"TableName": os.environ["TABLE"], "Key": marker,
                    "ConditionExpression": "ownerUserId=:owner", "ExpressionAttributeValues": {":owner": owner}}}
                    for marker in (account_lifecycle.owner_key(owner), account_lifecycle.run_key(namespace(owner)))]
                try:
                    transact([abort, *checks])
                except Exception as exc:
                    if not r.conditional(exc):
                        raise
            return r.response(409, {"error": "Call admission was revoked; no call created"})
        form = {"To": item["toNumber"], "From": item["fromNumber"], "Twiml": twiml,
            "Timeout": 20, "TimeLimit": call_seconds, "Record": "true", "RecordingChannels": "dual",
            "StatusCallback": base + "status/" + token, "StatusCallbackMethod": "POST",
            "StatusCallbackEvent": ["initiated", "ringing", "answered", "completed"],
            "RecordingStatusCallback": base + "recording/" + token, "RecordingStatusCallbackMethod": "POST",
            "RecordingStatusCallbackEvent": ["completed", "absent"]}
        if live:
            # Recording starts after the signed answered-call bootstrap.
            for field in ('Record', 'RecordingChannels', 'RecordingStatusCallback', 'RecordingStatusCallbackMethod', 'RecordingStatusCallbackEvent'):
                form.pop(field, None)
        result = provider("/Calls.json", form)
        sid = result.get("sid", "")
        if not r.SID.fullmatch(sid) or result.get("account_sid") != os.environ["TWILIO_ACCOUNT_SID"]:
            raise ProviderError()
        item = change(key, {"providerStatus": result.get("status") if result.get("status") in STATUSES else "queued"}, "provider_accepted", {"callSid": sid}, sid=sid)
    except ProviderError as exc:
        fields = {"providerStatus": "rejected" if exc.rejected else "unknown"}
        if exc.rejected:
            fields["recordingCreateState"] = "rejected"
        if exc.code:
            fields["providerCode"] = exc.code
        item = change(key, fields, "provider_create_outcome", fields, release=exc.rejected)
    return r.response(200, view(item))


def callback(path, raw, headers):
    match = re.fullmatch(r"/owner-message/(status|played|recording)/([0-9a-f]{64})", path)
    if not match:
        return r.response(404)
    kind, token = match.groups()
    pairs = urllib.parse.parse_qs(raw.decode(), keep_blank_values=True, max_num_fields=60)
    signed = os.environ["PUBLIC_BASE_URL"].rstrip("/") + path
    for key in sorted(pairs):
        for value in sorted(set(pairs[key])):
            signed += key + value
    expected = base64.b64encode(hmac.new(os.environ["TWILIO_AUTH_TOKEN"].encode(), signed.encode(), hashlib.sha1).digest()).decode()
    if not hmac.compare_digest(expected, headers.get("x-twilio-signature", "")):
        return r.response(403)
    if any(len(v) != 1 for v in pairs.values()):
        return r.response(400)
    data = {k: v[0] for k, v in pairs.items()}
    sid = data.get("CallSid", "")
    if data.get("AccountSid") != os.environ["TWILIO_ACCOUNT_SID"] or not r.SID.fullmatch(sid):
        return r.response(400)
    binding = r.get({"PK": "OWNER_CALLBACK#" + token, "SK": "META"})
    if not binding:
        return r.response(404)
    key = request_key(binding["ownerUserId"], binding["requestId"])
    item = r.get(key)
    if not item or item.get("callSid", sid) != sid or item["callbackId"] != token:
        return r.response(409)
    if any(data.get(field) is not None and data[field] != item[stored] for field, stored in (("To", "toNumber"), ("From", "fromNumber"))):
        return r.response(409)
    if kind == "status":
        status = data.get("CallStatus")
        if status not in STATUSES:
            return r.response(400)
        if item.get('live') is True and status in TERMINAL:
            import inbound_owner
            item = change(key, {}, 'provider_terminal_binding', {'callSid': sid}, sid=sid)
            inbound_owner.end(item, item.get('refusalReason', 'provider_terminal'), provider_status=status)
            return r.response(200)
        item = change(key, {"providerStatus": status}, "status", {"callSid": sid, "status": status, "duration": data.get("CallDuration")}, sid=sid, release=status in TERMINAL)
        if status in {"busy", "failed", "no-answer", "canceled"}:
            call = r.get(r.call_key(sid, namespace(item["ownerUserId"])))
            if call and call["recordingStatus"] == "pending" and not call.get("recordingSid"):
                new = {**call, "recordingStatus": "absent", "recordingReason": "call_" + status, "version": int(call["version"]) + 1}
                try:
                    r.commit(call, new)
                except Exception as exc:
                    if not r.conditional(exc):
                        raise
                    return r.response(503, {"error": "Recording state changed; retry callback"})
        return r.response(200)
    if kind == "played":
        change(key, {"playbackCompleted": True}, "playback_completed", {"callSid": sid}, sid=sid)
        return {"statusCode": 200, "headers": {"content-type": "text/xml", "cache-control": "no-store"}, "body": "<Response><Hangup/></Response>"}
    recording = data.get("RecordingSid", "")
    if not r.REC.fullmatch(recording) or data.get("RecordingStatus") not in {"completed", "absent"}:
        return r.response(400)
    import account_lifecycle
    call = r.get(r.call_key(sid, namespace(item["ownerUserId"])))
    if call and account_lifecycle.blocked(call):
        return account_lifecycle.resolve_deleted_recording(call, recording, data["RecordingStatus"])
    item = change(key, {}, "recording_callback", {"callSid": sid, "recordingSid": recording, "status": data["RecordingStatus"]}, sid=sid)
    run_id = namespace(item["ownerUserId"])
    return r.ingest_recording(sid, recording, data["RecordingStatus"], run_id=run_id)
