"""Private trial journal and recording delivery. Never log request bodies or tokens."""
import base64
import hashlib
import hmac
import json
import os
import re
import tempfile
import time
import urllib.parse
import urllib.request
from decimal import Decimal

MAX_AUDIO = 200 * 1024 * 1024
COPY_ATTEMPTS = 3
# Owner decision 2026-10-19: delete the Twilio copy once the S3 object is
# verified. Same three-attempt ceiling as the copy itself.
PROVIDER_DELETE_ATTEMPTS = 3
PROVIDER_DELETE_LEASE_SECONDS = 60
# Longer than the deployed recorder Lambda's 60-second execution limit.
COPY_LEASE_SECONDS = 120
LINK_SECONDS = 7 * 86400
SID = re.compile(r"^CA[0-9a-fA-F]{32}$")
REC = re.compile(r"^RE[0-9a-fA-F]{32}$")
TYPES = {"call_registered", "consent", "recording_requested", "transcript", "summary", "usage", "error", "call_ended"}
_table = _s3 = None


def clients():
    global _table, _s3
    if _table is None:
        import boto3
        _table = boto3.resource("dynamodb").Table(os.environ["TABLE"])
        _s3 = boto3.client("s3")
    return _table, _s3


def response(code, payload=None, headers=None):
    return {"statusCode": code, "headers": {"content-type": "application/json", "cache-control": "no-store", **(headers or {})},
            "body": json.dumps(payload or {}, default=lambda n: (int(n) if n == n.to_integral_value() else float(n)) if isinstance(n, Decimal) else str(n))}


def digest(raw):
    return hmac.new(os.environ["TWILIO_AUTH_TOKEN"].encode(), raw, hashlib.sha256).hexdigest()


def get(key):
    return clients()[0].get_item(Key=key, ConsistentRead=True).get("Item")


def call_key(sid, run_id=None):
    return {"PK": "RUN#" + (run_id or os.environ["RUN_ID"]), "SK": "CALL#" + sid}


def conditional(exc):
    return getattr(exc, "response", {}).get("Error", {}).get("Code") in {"ConditionalCheckFailedException", "TransactionCanceledException"}


def commit(old, new, event=None):
    """Optimistic version check; journal entry and state change are one transaction."""
    # The resource client's DynamoDB transform serializes native Python values.
    put = {"TableName": os.environ["TABLE"], "Item": new,
           "ConditionExpression": "#v = :v" if old else "attribute_not_exists(PK)"}
    if old:
        put.update(ExpressionAttributeNames={"#v": "version"}, ExpressionAttributeValues={":v": old["version"]})
    operations = [{"Put": put}]
    if new.get("runId"):
        import account_lifecycle
        operations.extend(account_lifecycle.ready_conditions(new))
    if event:
        operations.append({"Put": {"TableName": os.environ["TABLE"], "Item": event, "ConditionExpression": "attribute_not_exists(PK)"}})
    clients()[0].meta.client.transact_write_items(TransactItems=operations)


def event_post(raw):
    data = json.loads(raw, parse_float=Decimal)
    sid = data.get("callSid", "")
    if data.get("runId") != os.environ["RUN_ID"] or not SID.fullmatch(sid) or data.get("type") not in TYPES:
        return response(400, {"error": "Invalid event identity"})
    import account_lifecycle
    if account_lifecycle.blocked({"runId": data["runId"]}):
        return response(410, {"recordingStatus": "deleted"})
    event_id = hashlib.sha256(raw).hexdigest()
    key = call_key(sid)
    journal_key = {"PK": key["PK"], "SK": "EVENT#" + sid + "#" + event_id}
    if get(journal_key):
        return response(200 if research_notify(get(key)) else 503, {"duplicate": True})
    for _ in range(3):
        old = get(key)
        if not old and data["type"] != "call_registered":
            return response(409, {"error": "Call must be registered"})
        target = data.get("targetId")
        if not old and (not isinstance(target, str) or not 1 <= len(target) <= 200):
            return response(400, {"error": "targetId required"})
        if old and target is not None and target != old["targetId"]:
            return response(409, {"error": "Call target is immutable"})
        new = dict(old or {**key, "runId": data["runId"], "callSid": sid, "targetId": target, "recordingStatus": "pending"})
        new.update(version=int((old or {}).get("version", 0)) + 1, updatedAt=int(time.time()), lastEventType=data["type"])
        details = data.get("data", {})
        if not isinstance(details, dict):
            return response(400, {"error": "Event data must be an object"})
        if data["type"] == "recording_requested":
            outcome = details.get("outcome")
            if outcome not in {"attempted", "confirmed", "rejected"}:
                return response(400, {"error": "Invalid recording creation outcome"})
            if new.get("recordingCreateState") not in {"confirmed", "resolved"}:
                new["recordingCreateState"] = outcome
        if data["type"] == "consent":
            result = details.get("result")
            if result not in {"granted", "declined"}:
                return response(400, {"error": "Invalid consent result"})
            new["consentStatus"] = result
            if result == "declined" and new["recordingStatus"] not in {"ready", "deleted", "expired"}:
                new.update(recordingStatus="absent", recordingReason="consent_declined")
        if data["type"] == "error" and details.get("code") == "recording_start_failed" and new["recordingStatus"] == "pending":
            new.update(recordingStatus="failed", recordingReason="recording_start_failed")
        status = details.get("status")
        if (data["type"] == "call_ended" and status in {"no-answer", "busy", "canceled", "failed"}
                and not new.get("consentStatus") and not new.get("recordingSid") and new["recordingStatus"] == "pending"):
            new.update(recordingStatus="absent", recordingReason="call_" + status)
        if (data["type"] == "call_ended" and details.get("reason") in {"navigation_limit", "unsupported_menu", "voicemail"}
                and not new.get("consentStatus") and not new.get("recordingSid") and new["recordingStatus"] == "pending"):
            new.update(recordingStatus="absent", recordingReason=details["reason"])
        journal = {**journal_key, "createdAt": int(time.time()), "data": data}
        try:
            commit(old, new, journal)
            return response(200 if research_notify(new) else 503, {"accepted": True})
        except Exception as exc:
            if not conditional(exc):
                raise
            if get(journal_key):
                return response(200, {"duplicate": True})
    return response(409, {"error": "Concurrent update; retry"})


def research_notify(call):
    if not call or not call.get('phoneResearchJobId'):
        return True
    import phone_research
    return phone_research.event_updated(call)


def set_recording(sid, recording, status, run_id=None, expected_attempt=None, **fields):
    for _ in range(3):
        old = get(call_key(sid, run_id))
        if not old or old.get("recordingSid", recording) != recording:
            raise ValueError("Recording binding mismatch")
        import account_lifecycle
        if account_lifecycle.blocked(old):
            return {**old, "recordingStatus": "deleted"}
        if old.get("recordingStatus") in {"ready", "deleted", "expired"}:
            return old
        if expected_attempt is not None and old.get("copyAttempts") != expected_attempt:
            return old
        if expected_attempt is None and old.get("recordingStatus") == "copying":
            return old
        new = {**old, "recordingSid": recording, "recordingStatus": status,
               "version": int(old["version"]) + 1, **fields}
        if status == "ready":
            new.pop("recordingError", None)
            new.pop("recordingReason", None)
        try:
            commit(old, new)
            return new
        except Exception as exc:
            if not conditional(exc):
                raise
    raise RuntimeError("Concurrent recording update")


def recording_creation(sid, state, run_id=None):
    """Persist intent before a provider request; a timeout is not a rejection."""
    if state not in {"attempted", "confirmed", "rejected"}:
        raise ValueError("Invalid recording creation state")
    import account_lifecycle
    for _ in range(3):
        old = get(call_key(sid, run_id))
        if not old or account_lifecycle.blocked(old):
            raise ValueError("Recording call unavailable")
        if old.get("recordingCreateState") in {"confirmed", "resolved"}:
            return
        new = {**old, "version": int(old["version"]) + 1, "recordingCreateState": state}
        try:
            commit(old, new)
            return
        except Exception as exc:
            if not conditional(exc):
                raise
    raise RuntimeError("Concurrent recording intent update")


def ingest_recording(sid, recording, status, run_id=None):
    """One durable retry budget across callbacks and explicit reconciliation."""
    for _ in range(3):
        old = get(call_key(sid, run_id))
        if not old or old.get("recordingSid", recording) != recording:
            return response(409, {"error": "Recording binding mismatch"})
        import account_lifecycle
        if account_lifecycle.blocked(old):
            return response(200, {"recordingStatus": "deleted"})
        state = old.get("recordingStatus")
        if state in {"ready", "deleted", "expired"}:
            retry = state == "ready" and release_provider_copy(sid, recording, run_id=run_id) == "retry"
            return response(200 if research_notify(old) and not retry else 503, {"recordingStatus": state})
        if status not in {"completed", "absent"}:
            return response(400)
        now = int(time.time())
        if state == "copying" and int(old.get("copyLeaseUntil", 0)) > now:
            return response(503, {"error": "Recording copy in progress; retry"})
        if status == "absent":
            # A delayed absence event cannot undo a completed-media transfer
            # attempt or erase its visible failure and retry budget.
            if old.get("copyAttempts", 0):
                return response(200, {"recordingStatus": state})
            set_recording(sid, recording, "absent", run_id=run_id)
            return response(200)
        attempts = int(old.get("copyAttempts", 0))
        if attempts >= COPY_ATTEMPTS:
            set_recording(sid, recording, "failed", run_id=run_id, expected_attempt=attempts,
                          recordingError="Recording copy attempt limit reached", recordingReason="copy_attempt_limit")
            return response(200, {"recordingStatus": "failed", "error": "Recording copy attempt limit reached"})
        if int(old.get("copyRetryAt", 0)) > now:
            return response(503, {"error": "Recording copy retry delay", "retryAt": int(old["copyRetryAt"])})
        claimed = {**old, "recordingSid": recording, "recordingStatus": "copying",
                   "version": int(old["version"]) + 1, "copyAttempts": attempts + 1,
                   "copyLeaseUntil": now + COPY_LEASE_SECONDS, "updatedAt": now}
        try:
            commit(old, claimed)
            break
        except Exception as exc:
            if not conditional(exc):
                raise
    else:
        return response(503, {"error": "Concurrent recording update; retry"})
    attempt = attempts + 1
    try:
        key, size = copy_audio(sid, recording, run_id=run_id)
        result = set_recording(sid, recording, "ready", run_id=run_id, expected_attempt=attempt,
                               s3Key=key, audioBytes=size, sourceRetained=True, copyLeaseUntil=0,
                               providerCopyState="pending")
    except Exception:
        exhausted = attempt >= COPY_ATTEMPTS
        # A failed or unverified copy never deletes the provider audio.
        set_recording(sid, recording, "failed", run_id=run_id, expected_attempt=attempt,
                      sourceRetained=True, providerCopyState="retained",
                      copyLeaseUntil=0, copyRetryAt=int(time.time()) + (30 if attempt == 1 else 60),
                      recordingReason="copy_attempt_limit" if exhausted else "copy_failed",
                      recordingError="Recording copy attempt limit reached" if exhausted else "Audio copy failed; retry callback")
        return response(200 if exhausted else 503, {"recordingStatus": "failed", "error": "Audio copy failed"})
    retry = result.get("recordingStatus") == "ready" and release_provider_copy(sid, recording, run_id=run_id) == "retry"
    return response(200 if research_notify(result) and not retry else 503, {"recordingStatus": result["recordingStatus"]})


def provider_copy_update(old, drop=(), **fields):
    new = {**old, "version": int(old["version"]) + 1, **fields}
    for name in drop:
        new.pop(name, None)
    try:
        commit(old, new)
        return new
    except Exception as exc:
        if not conditional(exc):
            raise
        return None


def verified_s3_copy(call):
    """Re-check the stored object immediately before any provider deletion."""
    expected = f"phone-trial/{call['runId']}/{call['callSid']}/{call['recordingSid']}.mp3"
    size = call.get("audioBytes")
    if call.get("s3Key") != expected or not isinstance(size, (int, Decimal)) or int(size) <= 0:
        return False
    try:
        head = clients()[1].head_object(Bucket=os.environ["BUCKET"], Key=expected)
    except Exception:
        return False
    return (head.get("ContentLength") == int(size) and head.get("ContentType") == "audio/mpeg"
            and re.fullmatch(r"[a-f0-9]{64}", head.get("Metadata", {}).get("sha256", "")) is not None)


def delete_provider_audio(sid, recording):
    """Identity-checked DELETE; a 404 or already-deleted resource counts as deleted."""
    import account_lifecycle
    return account_lifecycle.provider_delete(sid, recording)


def release_provider_copy(sid, recording, run_id=None):
    """Delete the Twilio copy of a ready, S3-verified recording.

    Returns "deleted", "retained" (verification failed or attempts exhausted;
    the provider copy stays), "busy" (another worker holds the lease),
    "retry" (a later callback may retry) or "skipped".
    """
    import account_lifecycle
    for _ in range(3):
        old = get(call_key(sid, run_id))
        if (not old or old.get("recordingSid") != recording or old.get("recordingStatus") != "ready"
                or account_lifecycle.blocked(old)):
            return "skipped"
        state = old.get("providerCopyState")
        if state == "deleted":
            return "deleted"
        if state not in {"pending", "deleting"}:
            return "retained" if state else "skipped"
        now = int(time.time())
        if state == "deleting" and int(old.get("providerCopyLeaseUntil", 0)) > now:
            return "busy"
        attempts = int(old.get("providerCopyDeleteAttempts", 0))
        if attempts >= PROVIDER_DELETE_ATTEMPTS:
            if provider_copy_update(old, providerCopyState="retained", providerCopyLeaseUntil=0,
                                    providerCopyReason="delete_attempt_limit",
                                    providerCopyError="Provider copy deletion attempt limit reached") is not None:
                return "retained"
            continue
        if int(old.get("providerCopyRetryAt", 0)) > now:
            return "retry"
        if not verified_s3_copy(old):
            if provider_copy_update(old, providerCopyState="retained", providerCopyLeaseUntil=0,
                                    providerCopyReason="s3_verification_failed",
                                    providerCopyError="Stored copy could not be verified; provider copy kept") is not None:
                return "retained"
            continue
        claimed = provider_copy_update(old, providerCopyState="deleting", providerCopyDeleteAttempts=attempts + 1,
                                       providerCopyLeaseUntil=now + PROVIDER_DELETE_LEASE_SECONDS)
        if claimed is not None:
            break
    else:
        return "retry"
    attempt = attempts + 1
    try:
        delete_provider_audio(sid, recording)
        fields = dict(providerCopyState="deleted", providerCopyDeletedAt=int(time.time()), sourceRetained=False,
                      providerCopyLeaseUntil=0)
        outcome = "deleted"
    except Exception:
        exhausted = attempt >= PROVIDER_DELETE_ATTEMPTS
        fields = dict(providerCopyState="retained" if exhausted else "pending", providerCopyLeaseUntil=0,
                      providerCopyRetryAt=int(time.time()) + (30 if attempt == 1 else 60),
                      providerCopyReason="delete_attempt_limit" if exhausted else "delete_failed",
                      providerCopyError=("Provider copy deletion attempt limit reached" if exhausted
                                         else "Provider copy deletion failed; retry callback"))
        outcome = "retained" if exhausted else "retry"
    for _ in range(3):
        current = get(call_key(sid, run_id))
        if (not current or current.get("providerCopyState") != "deleting"
                or int(current.get("providerCopyDeleteAttempts", 0)) != attempt):
            return outcome
        drop = ("providerCopyError", "providerCopyReason", "providerCopyRetryAt") if outcome == "deleted" else ()
        if provider_copy_update(current, drop=drop, **fields) is not None:
            return outcome
    return outcome


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def copy_audio(sid, recording, run_id=None):
    import account_lifecycle
    call = get(call_key(sid, run_id)) or {"runId": run_id or os.environ["RUN_ID"], "callSid": sid}
    call = {**call, "recordingSid": recording}
    if account_lifecycle.blocked(call):
        raise ValueError("Recording owner deleted")
    account = os.environ["TWILIO_ACCOUNT_SID"]
    url = f"https://api.twilio.com/2010-04-01/Accounts/{account}/Recordings/{recording}.mp3"
    auth = base64.b64encode((os.environ.get("TWILIO_API_KEY_SID", account) + ":" + os.environ.get("TWILIO_API_KEY_SECRET", os.environ["TWILIO_AUTH_TOKEN"])).encode()).decode()
    req = urllib.request.Request(url, headers={"Authorization": "Basic " + auth})
    key = f"phone-trial/{run_id or os.environ['RUN_ID']}/{sid}/{recording}.mp3"
    # No redirects: provider credentials must never reach another host.
    with urllib.request.build_opener(NoRedirect).open(req, timeout=20) as source, tempfile.TemporaryFile() as audio:
        if source.headers.get_content_type() not in {"audio/mpeg", "audio/mp3"}:
            raise ValueError("Unexpected audio type")
        declared_length = source.headers.get("Content-Length")
        if declared_length is not None:
            if not re.fullmatch(r"[0-9]+", declared_length):
                raise ValueError("Invalid audio content length")
            declared_length = int(declared_length)
            if declared_length > MAX_AUDIO:
                raise ValueError("Audio exceeds size limit")
        size = 0
        checksum = hashlib.sha256()
        deadline = time.monotonic() + 30
        while True:
            if time.monotonic() >= deadline:
                raise TimeoutError("Audio copy deadline")
            block = source.read(1024 * 1024)
            if not block:
                break
            size += len(block)
            if size > MAX_AUDIO:
                raise ValueError("Audio exceeds size limit")
            checksum.update(block)
            audio.write(block)
        if size == 0:
            raise ValueError("Empty audio")
        if declared_length is not None and size != declared_length:
            raise ValueError("Truncated audio content")
        audio_hash = checksum.hexdigest()
        audio.seek(0)
        if account_lifecycle.blocked(call):
            raise ValueError("Recording owner deleted")
        clients()[1].put_object(Bucket=os.environ["BUCKET"], Key=key, Body=audio,
                               ContentLength=size, ContentType="audio/mpeg", ServerSideEncryption="AES256",
                               ChecksumSHA256=base64.b64encode(checksum.digest()).decode(),
                               Metadata={"sha256": audio_hash})
    if account_lifecycle.cleanup_if_deleted(call, key):
        raise ValueError("Recording owner deleted during transfer")
    head = clients()[1].head_object(Bucket=os.environ["BUCKET"], Key=key)
    if (head["ContentLength"] != size or head.get("ContentType") != "audio/mpeg"
            or head.get("Metadata", {}).get("sha256") != audio_hash):
        raise ValueError("S3 verification failed")
    return key, size


def recording_post(raw, headers):
    pairs = urllib.parse.parse_qs(raw.decode(), keep_blank_values=True, max_num_fields=40)
    # Twilio's canonical form signature includes sorted unique values for each name.
    signed = os.environ["PUBLIC_BASE_URL"].rstrip("/") + "/recording"
    for key in sorted(pairs):
        for value in sorted(set(pairs[key])):
            signed += key + value
    expected = base64.b64encode(hmac.new(os.environ["TWILIO_AUTH_TOKEN"].encode(), signed.encode(), hashlib.sha1).digest()).decode()
    if not hmac.compare_digest(expected, headers.get("x-twilio-signature", "")):
        return response(403)
    if any(len(v) != 1 for v in pairs.values()):
        return response(400)
    data = {k: v[0] for k, v in pairs.items()}
    sid, recording = data.get("CallSid", ""), data.get("RecordingSid", "")
    if data.get("AccountSid") != os.environ["TWILIO_ACCOUNT_SID"] or not SID.fullmatch(sid) or not REC.fullmatch(recording):
        return response(400)
    import account_lifecycle
    call = get(call_key(sid))
    if call and account_lifecycle.blocked(call):
        return account_lifecycle.resolve_deleted_recording(call, recording, data.get("RecordingStatus"))
    return ingest_recording(sid, recording, data.get("RecordingStatus"))


def issue_link(call):
    table = clients()[0]
    # Fixed issuance time survives reports/retries. Conditional write resolves races.
    key = {"PK": "LINK#" + call["recordingSid"], "SK": "META"}
    metadata = get(key)
    if not metadata:
        metadata = {**key, "issuedAt": int(time.time()), "runId": call["runId"], "callSid": call["callSid"], "recordingSid": call["recordingSid"], "s3Key": call["s3Key"]}
        metadata["expiresAt"] = metadata["issuedAt"] + LINK_SECONDS
        try:
            table.put_item(Item=metadata, ConditionExpression="attribute_not_exists(PK)")
        except Exception as exc:
            if not conditional(exc):
                raise
            metadata = get(key)
    if any(metadata[k] != call[k] for k in ("runId", "callSid", "recordingSid", "s3Key")):
        raise ValueError("Link binding mismatch")
    token = digest(("recording-link\n" + call["runId"] + "\n" + call["callSid"] + "\n" + call["recordingSid"] + "\n" + str(metadata["issuedAt"])).encode())
    token_key = {"PK": "TOKEN#" + hashlib.sha256(token.encode()).hexdigest(), "SK": "META"}
    table.put_item(Item={**metadata, **token_key, "ttlAt": metadata["expiresAt"]})
    return {"url": os.environ["PUBLIC_BASE_URL"].rstrip("/") + "/download?token=" + token, "expiresAt": metadata["expiresAt"]}


def report_post(raw, include_links=True, call_sids=None):
    if json.loads(raw).get("runId") != os.environ["RUN_ID"]:
        return response(400)
    if call_sids is not None and (not isinstance(call_sids, list) or len(call_sids) > 20
            or any(not isinstance(sid, str) or not SID.fullmatch(sid) for sid in call_sids)
            or len(set(call_sids)) != len(call_sids)):
        return response(400, {"error": "callSids must be at most 20 distinct CallSids"})
    table = clients()[0]
    args = {"KeyConditionExpression": "PK = :pk", "ExpressionAttributeValues": {":pk": "RUN#" + os.environ["RUN_ID"]}, "ConsistentRead": True}
    items = []
    while True:
        page = table.query(**args)
        items.extend(page["Items"])
        if len(items) > 5000:
            return response(413, {"error": "Report exceeds trial limit"})
        if "LastEvaluatedKey" not in page:
            break
        args["ExclusiveStartKey"] = page["LastEvaluatedKey"]
    calls = [i for i in items if i["SK"].startswith("CALL#")]
    events = [i for i in items if i["SK"].startswith("EVENT#")]
    if call_sids is not None:
        calls = [call for call in calls if call["callSid"] in call_sids]
        events = [event for event in events if event.get("data", {}).get("callSid") in call_sids]
    for call in calls:
        if include_links and call.get("recordingStatus") == "ready":
            call["recordingLink"] = issue_link(call)
    control = get({"PK": "TRIAL#" + os.environ["RUN_ID"], "SK": "CONTROL"})
    return response(200, {"runId": os.environ["RUN_ID"], "calls": calls, "events": events, "control": control})


def download(query):
    token = query.get("token", "")
    if not re.fullmatch(r"[0-9a-f]{64}", token):
        return response(404)
    metadata = get({"PK": "TOKEN#" + hashlib.sha256(token.encode()).hexdigest(), "SK": "META"})
    if not metadata or int(metadata["expiresAt"]) <= int(time.time()):
        return response(404)
    if metadata["runId"] != os.environ["RUN_ID"] and not re.fullmatch(r"owner-messages#[0-9a-f-]{36}", metadata["runId"]):
        return response(404)
    call = get(call_key(metadata["callSid"], metadata["runId"]))
    if not call or call.get("recordingStatus") != "ready" or any(call.get(k) != metadata[k] for k in ("runId", "recordingSid", "s3Key")):
        return response(404)
    ttl = min(900, int(metadata["expiresAt"]) - int(time.time()))
    if ttl <= 0:
        return response(404)
    url = clients()[1].generate_presigned_url("get_object", Params={"Bucket": os.environ["BUCKET"], "Key": metadata["s3Key"],
        "ResponseContentType": "audio/mpeg", "ResponseContentDisposition": 'attachment; filename="' + metadata["callSid"] + '.mp3"'}, ExpiresIn=ttl)
    return response(302, headers={"location": url, "referrer-policy": "no-referrer"})


def reconcile(event):
    """IAM-only recovery verifies provider metadata before copying known call audio."""
    sid, recording = event.get("callSid", ""), event.get("recordingSid", "")
    if event.get("runId") != os.environ["RUN_ID"] or not SID.fullmatch(sid) or not REC.fullmatch(recording):
        return response(400)
    if not get(call_key(sid)):
        return response(409)
    account = os.environ["TWILIO_ACCOUNT_SID"]
    auth = base64.b64encode((os.environ.get("TWILIO_API_KEY_SID", account) + ":" + os.environ.get("TWILIO_API_KEY_SECRET", os.environ["TWILIO_AUTH_TOKEN"])).encode()).decode()
    url = f"https://api.twilio.com/2010-04-01/Accounts/{account}/Recordings/{recording}.json"
    try:
        with urllib.request.build_opener(NoRedirect).open(urllib.request.Request(url, headers={"Authorization": "Basic " + auth}), timeout=10) as source:
            raw = source.read(65537)
            if len(raw) > 65536:
                return response(502)
            metadata = json.loads(raw)
    except Exception:
        return response(503, {"error": "Provider metadata unavailable; retry"})
    if metadata.get("account_sid") != account or metadata.get("call_sid") != sid or metadata.get("sid") != recording:
        return response(409, {"error": "Provider recording binding mismatch"})
    data = {"AccountSid": account, "CallSid": sid, "RecordingSid": recording, "RecordingStatus": metadata.get("status", "")}
    signed = os.environ["PUBLIC_BASE_URL"].rstrip("/") + "/recording" + "".join(k + data[k] for k in sorted(data))
    signature = base64.b64encode(hmac.new(os.environ["TWILIO_AUTH_TOKEN"].encode(), signed.encode(), hashlib.sha1).digest()).decode()
    return recording_post(urllib.parse.urlencode(data).encode(), {"x-twilio-signature": signature})


def handler(event, context):
    # Only a principal with lambda:InvokeFunction can supply a direct outer event.
    # API Gateway always supplies requestContext/version, regardless of HTTP body.
    if "requestContext" not in event and "version" not in event:
        if event.get('operation') in {'phone_research_catalog', 'phone_research_advance', 'phone_research_status'}:
            import phone_research
            return phone_research.operation(event)
        if event.get("operation") in {"owner_account_export", "owner_account_purge"}:
            import account_lifecycle
            return account_lifecycle.operation(event)
        if event.get("operation") in {"owner_recordings_list", "owner_recording_media"}:
            import owner_recordings
            return owner_recordings.operation(event)
        if event.get("operation") in {"owner_message_send", "owner_message_status"}:
            import owner_messages
            return owner_messages.operation(event)
        if event.get("operation") in {"owner_budget_status", "owner_budget_grant"}:
            import owner_messages
            return owner_messages.budget_operation(event)
        if event.get("operation") == "internal_report":
            return report_post(json.dumps({"runId": event.get("runId")}).encode(), include_links=event.get("includeLinks", True) is True, call_sids=event.get("callSids"))
        if event.get("operation") == "internal_reconcile":
            return reconcile(event)
    raw = event.get("body") or ""
    raw = base64.b64decode(raw) if event.get("isBase64Encoded") else raw.encode()
    if len(raw) > 24000:
        return response(413)
    headers = {k.lower(): v for k, v in event.get("headers", {}).items()}
    path = event.get("rawPath", "")
    method = event.get("requestContext", {}).get("http", {}).get("method", "")
    try:
        if path.startswith('/phone-research/') and method == 'POST':
            import phone_research
            return phone_research.callback(path, raw, headers)
        if path.startswith("/inbound-owner/") and method == "POST":
            import inbound_owner
            if path in {"/inbound-owner/redeem", "/inbound-owner/check", "/inbound-owner/finish"}:
                return inbound_owner.redeem(raw, headers, path.rsplit("/", 1)[1])
            return inbound_owner.callback(path, raw, headers)
        if path.startswith("/owner-message/") and method == "POST":
            import owner_messages
            return owner_messages.callback(path, raw, headers)
        if path == "/recording" and method == "POST":
            return recording_post(raw, headers)
        if path == "/download" and method == "GET":
            return download(event.get("queryStringParameters") or {})
        if path in {"/events", "/report"} and method == "POST":
            if not hmac.compare_digest(digest(raw), headers.get("x-ln-signature", "")):
                return response(403)
            return event_post(raw) if path == "/events" else report_post(raw)
        return response(404)
    except (ValueError, TypeError, KeyError):
        return response(400, {"error": "Invalid request"})
