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
LINK_SECONDS = 7 * 86400
SID = re.compile(r"^CA[0-9a-fA-F]{32}$")
REC = re.compile(r"^RE[0-9a-fA-F]{32}$")
TYPES = {"call_registered", "consent", "transcript", "summary", "usage", "error", "call_ended"}
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
            "body": json.dumps(payload or {}, default=lambda n: float(n) if isinstance(n, Decimal) else str(n))}


def digest(raw):
    return hmac.new(os.environ["TWILIO_AUTH_TOKEN"].encode(), raw, hashlib.sha256).hexdigest()


def get(key):
    return clients()[0].get_item(Key=key, ConsistentRead=True).get("Item")


def call_key(sid):
    return {"PK": "RUN#" + os.environ["RUN_ID"], "SK": "CALL#" + sid}


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
    if event:
        operations.append({"Put": {"TableName": os.environ["TABLE"], "Item": event, "ConditionExpression": "attribute_not_exists(PK)"}})
    clients()[0].meta.client.transact_write_items(TransactItems=operations)


def event_post(raw):
    data = json.loads(raw, parse_float=Decimal)
    sid = data.get("callSid", "")
    if data.get("runId") != os.environ["RUN_ID"] or not SID.fullmatch(sid) or data.get("type") not in TYPES:
        return response(400, {"error": "Invalid event identity"})
    event_id = hashlib.sha256(raw).hexdigest()
    key = call_key(sid)
    journal_key = {"PK": key["PK"], "SK": "EVENT#" + sid + "#" + event_id}
    if get(journal_key):
        return response(200, {"duplicate": True})
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
        if data["type"] == "consent":
            result = details.get("result")
            if result not in {"granted", "declined"}:
                return response(400, {"error": "Invalid consent result"})
            new["consentStatus"] = result
            if result == "declined" and new["recordingStatus"] != "ready":
                new.update(recordingStatus="absent", recordingReason="consent_declined")
        if data["type"] == "error" and details.get("code") == "recording_start_failed" and new["recordingStatus"] == "pending":
            new.update(recordingStatus="failed", recordingReason="recording_start_failed")
        status = details.get("status")
        if (data["type"] == "call_ended" and status in {"no-answer", "busy", "canceled", "failed"}
                and not new.get("consentStatus") and not new.get("recordingSid") and new["recordingStatus"] == "pending"):
            new.update(recordingStatus="absent", recordingReason="call_" + status)
        journal = {**journal_key, "createdAt": int(time.time()), "data": data}
        try:
            commit(old, new, journal)
            return response(200, {"accepted": True})
        except Exception as exc:
            if not conditional(exc):
                raise
            if get(journal_key):
                return response(200, {"duplicate": True})
    return response(409, {"error": "Concurrent update; retry"})


def set_recording(sid, recording, status, **fields):
    for _ in range(3):
        old = get(call_key(sid))
        if not old or old.get("recordingSid", recording) != recording:
            raise ValueError("Recording binding mismatch")
        if old.get("recordingStatus") == "ready":
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


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def copy_audio(sid, recording):
    account = os.environ["TWILIO_ACCOUNT_SID"]
    url = f"https://api.twilio.com/2010-04-01/Accounts/{account}/Recordings/{recording}.mp3"
    auth = base64.b64encode((os.environ.get("TWILIO_API_KEY_SID", account) + ":" + os.environ.get("TWILIO_API_KEY_SECRET", os.environ["TWILIO_AUTH_TOKEN"])).encode()).decode()
    req = urllib.request.Request(url, headers={"Authorization": "Basic " + auth})
    key = f"phone-trial/{os.environ['RUN_ID']}/{sid}/{recording}.mp3"
    # No redirects: provider credentials must never reach another host.
    with urllib.request.build_opener(NoRedirect).open(req, timeout=20) as source, tempfile.TemporaryFile() as audio:
        if source.headers.get_content_type() not in {"audio/mpeg", "audio/mp3"}:
            raise ValueError("Unexpected audio type")
        if int(source.headers.get("Content-Length", "0")) > MAX_AUDIO:
            raise ValueError("Audio exceeds size limit")
        size = 0
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
            audio.write(block)
        if size == 0:
            raise ValueError("Empty audio")
        audio.seek(0)
        clients()[1].put_object(Bucket=os.environ["BUCKET"], Key=key, Body=audio,
                               ContentLength=size, ContentType="audio/mpeg", ServerSideEncryption="AES256")
    head = clients()[1].head_object(Bucket=os.environ["BUCKET"], Key=key)
    if head["ContentLength"] != size:
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
    old = get(call_key(sid))
    if not old or old.get("recordingSid", recording) != recording:
        return response(409)
    if old.get("recordingStatus") == "ready":
        return response(200, {"duplicate": True})
    status = data.get("RecordingStatus")
    if status == "absent":
        set_recording(sid, recording, "absent")
        return response(200)
    if status != "completed":
        return response(400)
    set_recording(sid, recording, "pending")
    try:
        key, size = copy_audio(sid, recording)
        set_recording(sid, recording, "ready", s3Key=key, audioBytes=size, sourceRetained=True)
        return response(200)
    except Exception:
        set_recording(sid, recording, "failed", recordingError="Audio copy failed; retry callback")
        return response(503, {"error": "Audio copy failed; retry"})


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


def report_post(raw, include_links=True):
    if json.loads(raw).get("runId") != os.environ["RUN_ID"]:
        return response(400)
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
    for call in calls:
        if include_links and call.get("recordingStatus") == "ready":
            call["recordingLink"] = issue_link(call)
    control = get({"PK": "TRIAL#" + os.environ["RUN_ID"], "SK": "CONTROL"})
    return response(200, {"runId": os.environ["RUN_ID"], "calls": calls, "events": [i for i in items if i["SK"].startswith("EVENT#")], "control": control})


def download(query):
    token = query.get("token", "")
    if not re.fullmatch(r"[0-9a-f]{64}", token):
        return response(404)
    metadata = get({"PK": "TOKEN#" + hashlib.sha256(token.encode()).hexdigest(), "SK": "META"})
    if not metadata or int(metadata["expiresAt"]) <= int(time.time()) or metadata["runId"] != os.environ["RUN_ID"]:
        return response(404)
    call = get(call_key(metadata["callSid"]))
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
        if event.get("operation") == "internal_report":
            return report_post(json.dumps({"runId": event.get("runId")}).encode(), include_links=event.get("includeLinks", True) is True)
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
