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
MAX_RESERVE = Decimal("1.40")
LIMIT = Decimal("20")
TERMINAL = {"completed", "busy", "failed", "no-answer", "canceled"}
STATUSES = TERMINAL | {"queued", "initiated", "ringing", "in-progress"}
INTRO = "This is Live Ninja, Jeremy's AI assistant. This owner message is being recorded. "
PHONE = re.compile(r"^\+[1-9][0-9]{7,14}$")
REQUEST_ID = re.compile(r"^[A-Za-z0-9_-]{1,100}$")


class ProviderError(Exception):
    def __init__(self, code=None, rejected=False):
        self.code, self.rejected = code, rejected


class InboundDailyLimit(ValueError):
    """The separately authorized incoming daily allowance cannot admit a call."""


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
               "recordingStatus": "pending" if item.get("callSid") else "absent", "reservedUSD": str(RESERVE)}
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


def budget_status(cfg):
    month, day, zone, expires = budget_clock(cfg)
    month_key, day_key = budget_keys(month, day)
    monthly, daily = r.get(month_key) or {}, r.get(day_key) or {}
    floor = Decimal(str(cfg.get("budgetFloorUSD", "8"))) if cfg.get("budgetFloorMonth") == month else Decimal("8")
    allocated = max(floor, Decimal(str(monthly.get("allocatedUSD", "0"))))
    granted, spent = (Decimal(str(daily.get(k, "0"))) for k in ("grantedUSD", "spentUSD"))
    lock = r.get(VOICE_KEY) or {}
    stopped = bool(lock.get("activeRequest"))
    extra = max(Decimal(0), allocated + RESERVE - max(LIMIT, allocated))
    reason = "disabled" if cfg.get("enabled") is not True else "voice_stop_or_active_call" if stopped else "waiting_approval" if granted - spent < extra else ""
    return {"month": month, "day": day, "timezone": zone, "allocatedUSD": str(allocated),
            "monthlyLimitUSD": str(LIMIT), "dailyGrantedUSD": str(granted), "dailySpentUSD": str(spent),
            "dailyRemainingUSD": str(max(Decimal(0), granted-spent)), "requiredReservationUSD": str(MAX_RESERVE), "expiresAt": expires,
            "pausedReason": reason, "pendingReservation": stopped,
            "incomingDailyBudget": inbound_budget_summary(cfg, day, zone, expires)}


def budget_operation(event):
    if event.get("operation") not in {"owner_budget_status", "owner_budget_grant"}:
        return r.response(400, {"error": "Unknown budget operation"})
    cfg = config(event.get("ownerUserId"))
    if not cfg:
        return r.response(403, {"error": "Owner budget is not configured for this identity"})
    if event["operation"] == "owner_budget_status":
        return r.response(200, budget_status(cfg))
    request_id = event.get("requestId", "")
    if not isinstance(request_id, str) or not REQUEST_ID.fullmatch(request_id):
        return r.response(400, {"error": "Invalid requestId"})
    month, day, zone, expires = budget_clock(cfg)
    if event.get("expectedDay") != day:
        return r.response(409, {"error": "Day changed; refresh the budget before approval"})
    grant_key = {"PK": "OWNER#" + cfg["ownerUserId"], "SK": "BUDGET_GRANT#" + request_id}
    _, day_key = budget_keys(month, day)
    for _ in range(4):
        previous = r.get(grant_key)
        if previous:
            if previous["day"] != day:
                return r.response(409, {"error": "Approval request belongs to an expired day"})
            return r.response(200, budget_status(cfg))
        if budget_clock(cfg)[1] != day:
            return r.response(409, {"error": "Day changed; refresh the budget before approval"})
        old = r.get(day_key)
        remaining = Decimal(str((old or {}).get("grantedUSD", "0"))) - Decimal(str((old or {}).get("spentUSD", "0")))
        if remaining >= MAX_RESERVE:
            return r.response(409, {"error": "Today's allowance covers the next supported call reservation; another approval is not needed"})
        daily = {**(old or {}), **day_key, "version": int((old or {}).get("version", 0))+1,
                 "grantedUSD": Decimal(str((old or {}).get("grantedUSD", "0")))+Decimal("5"),
                 "spentUSD": Decimal(str((old or {}).get("spentUSD", "0"))), "expiresAt": expires, "timezone": zone}
        config_check = {"TableName": os.environ["TABLE"], "Key": CONFIG_KEY,
                        "ConditionExpression": "ownerUserId=:owner",
                        "ExpressionAttributeNames": {"#zone": "timezone"},
                        "ExpressionAttributeValues": {":owner": cfg["ownerUserId"]}}
        if "timezone" in cfg:
            config_check["ConditionExpression"] += " AND #zone=:zone"
            config_check["ExpressionAttributeValues"][":zone"] = cfg["timezone"]
        else:
            config_check["ConditionExpression"] += " AND attribute_not_exists(#zone)"
        try:
            transact([put(daily, old), put({**grant_key, "day": day, "amountUSD": Decimal("5"),
                      "createdAt": int(time.time()), "expiresAt": expires}),
                      {"ConditionCheck": config_check}])
            return r.response(200, budget_status(cfg))
        except Exception as exc:
            if not r.conditional(exc):
                raise
    return r.response(409, {"error": "Budget changed; retry the same approval request"})


def reserve(owner, request_id, alias, message, cfg, *, reserve_amount=RESERVE, initial_fields=None, extra_checks=(), target_number=None, inbound_policy=None):
    usage = provider("/Usage/Records/ThisMonth.json?Category=totalprice")
    records = usage.get("usage_records", [])
    if len(records) != 1 or records[0].get("price_unit", "").lower() != "usd" or usage.get("next_page_uri"):
        raise ValueError("Monthly provider cost unavailable")
    observed = Decimal(str(records[0].get("price")))
    floor = Decimal(str(cfg.get("budgetFloorUSD", "8")))
    if not observed.is_finite() or observed < 0 or not floor.is_finite() or floor < 8:
        raise ValueError("Invalid monthly budget")
    month, day, zone, expires = budget_clock(cfg)
    floor_month = cfg.get("budgetFloorMonth")
    if floor_month is not None and (not isinstance(floor_month, str) or not re.fullmatch(r"[0-9]{4}-(0[1-9]|1[0-2])", floor_month)):
        raise ValueError("Invalid budget floor month")
    if floor > 8 and floor_month is None:
        raise ValueError("A budget floor above $8 requires budgetFloorMonth")
    if floor_month != month:
        floor = Decimal("8")
    budget_key, day_key = budget_keys(month, day)
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
                or request_id != 'inbound-' + item['callSid'] or target_number is not None or reserve_amount != MAX_RESERVE
                or inbound_daily_policy(cfg, {'ownerUserId': owner, 'dailyBudget': inbound_policy}, item['toNumber']) is None):
            raise ValueError('Recurring incoming allowance requires an exact inbound call')
    for _ in range(4):
        prior = r.get(key)
        if prior:
            return prior, False
        old = r.get(budget_key)
        base = max(floor, Decimal("8") + observed, Decimal(str((old or {}).get("allocatedUSD", "0"))))
        if budget_clock(cfg)[1] != day:
            raise ValueError("Day changed; retry call admission")
        daily_key = inbound_budget_key(owner, day) if inbound_policy is not None else day_key
        daily_old = r.get(daily_key)
        extra = max(Decimal(0), base + reserve_amount - max(LIMIT, base))
        daily = None
        if inbound_policy is not None:
            reserved = Decimal(str((daily_old or {}).get('reservedUSD', '0')))
            if (not reserved.is_finite() or reserved < 0 or reserved > Decimal('10')
                    or (daily_old and (daily_old.get('ownerUserId') != owner or daily_old.get('day') != day or daily_old.get('timezone') != zone))):
                raise ValueError('Incoming daily allowance ledger is invalid')
            if reserved + reserve_amount > Decimal('10'):
                raise InboundDailyLimit('Incoming daily allowance is used for today')
            daily = {**(daily_old or {}), **daily_key, 'ownerUserId': owner, 'day': day, 'timezone': zone,
                     'limitUSD': Decimal('10'), 'reservedUSD': reserved + reserve_amount, 'expiresAt': expires,
                     'version': int((daily_old or {}).get('version', 0)) + 1}
            item.update(budgetSource='incoming-daily-policy', inboundBudgetDay=day)
        elif extra:
            granted = Decimal(str((daily_old or {}).get("grantedUSD", "0")))
            daily_spent = Decimal(str((daily_old or {}).get("spentUSD", "0")))
            if not daily_old or int(daily_old.get("expiresAt", 0)) <= int(time.time()) or daily_spent + extra > granted:
                raise ValueError("waiting_approval: Monthly $20 allowance reached; approve $5 for today")
            daily = {**daily_old, "spentUSD": daily_spent + extra, "version": int(daily_old["version"])+1}
        budget = {**(old or {}), **budget_key, "allocatedUSD": base + reserve_amount,
                  "observedProviderUSD": observed, "observedAt": int(time.time()),
                  "version": int((old or {}).get("version", 0)) + 1}
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
        if daily:
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
    if alias not in {"primary", "secondary"} or not isinstance(message, str) or not 1 <= len(message.strip()) <= 1000 or len(message) > 1000 or any(ord(c) < 32 and c not in "\n\t" for c in message):
        return r.response(400, {"error": "Use a configured alias and 1–1000 characters of plain text"})
    if prior:
        if prior["destination"] != alias or prior["message"] != message:
            return r.response(409, {"error": "requestId already identifies a different message"})
        return r.response(200, view(prior))
    number = cfg.get("destinations", {}).get(alias, "")
    if not PHONE.fullmatch(number) or not PHONE.fullmatch(cfg.get("fromNumber", "")):
        return r.response(403, {"error": "Destination is not configured"})
    try:
        item, fresh = reserve(owner, request_id, alias, message, cfg)
    except ValueError as exc:
        return r.response(409, {"error": str(exc)})
    except ProviderError:
        return r.response(409, {"error": "Provider usage cannot be verified; no call created"})
    if not fresh:
        if item["destination"] != alias or item["message"] != message:
            return r.response(409, {"error": "requestId already identifies a different message"})
        return r.response(200, view(item))
    base = os.environ["PUBLIC_BASE_URL"].rstrip("/") + "/owner-message/"
    token = item["callbackId"]
    twiml = '<Response><Say>' + html.escape(item["scriptTranscript"]) + '</Say><Redirect method="POST">' + html.escape(base + "played/" + token) + '</Redirect></Response>'
    # Reserve ten seconds for the create request and twenty seconds for ringing.
    call_seconds = min(90, int(item.get("budgetExpiresAt", int(time.time())+120))-int(time.time())-30)
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
        result = provider("/Calls.json", {"To": item["toNumber"], "From": item["fromNumber"], "Twiml": twiml,
            "Timeout": 20, "TimeLimit": call_seconds, "Record": "true", "RecordingChannels": "dual",
            "StatusCallback": base + "status/" + token, "StatusCallbackMethod": "POST",
            "StatusCallbackEvent": ["initiated", "ringing", "answered", "completed"],
            "RecordingStatusCallback": base + "recording/" + token, "RecordingStatusCallbackMethod": "POST",
            "RecordingStatusCallbackEvent": ["completed", "absent"]})
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
