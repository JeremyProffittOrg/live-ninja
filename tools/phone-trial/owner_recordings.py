"""Owner-authenticated access to existing phone recordings; never creates calls."""
import os
import re
import recorder as r
import owner_messages as m

RUN = re.compile(r"^[a-z0-9][a-z0-9-]{0,99}$")
TTL = 300


def sources(cfg):
    # Legacy trial records lack ownerId. Only this explicit, private binding
    # permits access; RUN_ID alone is never an ownership assertion.
    runs = cfg.get("recordingRuns", [])
    if not isinstance(runs, list) or len(runs) > 20 or any(not isinstance(v, str) or not RUN.fullmatch(v) for v in runs):
        raise ValueError("Invalid recording source configuration")
    return ["messages"] + list(dict.fromkeys(runs))


def call_for(owner, source, sid):
    run = m.namespace(owner) if source == "messages" else source
    call = r.get(r.call_key(sid, run))
    if not call and source == 'messages':
        cfg = m.config(owner) or {}
        for candidate in sources(cfg)[1:]:
            bound = r.get(r.call_key(sid, candidate))
            if (bound and bound.get('ownerUserId') == owner and bound.get('phoneResearchJobId')
                    and bound.get('phoneResearchRunId') and bound.get('runId') == candidate and bound.get('callSid') == sid):
                return bound
    if not call or (source == "messages" and call.get("ownerUserId") != owner):
        return None
    if call.get("runId") != run or call.get("callSid") != sid:
        return None
    return call


def projection(call):
    state = call.get("recordingStatus", "pending")
    if state not in {"requested", "recording", "processing", "copying", "ready", "pending", "absent", "failed", "deleted", "expired"}:
        state = "failed"
    return {"callSid": call["callSid"], "target": call.get("targetId", ""),
            "recordingStatus": state, "reason": call.get("recordingReason", ""),
            "updatedAt": call.get("updatedAt", 0), "audioBytes": call.get("audioBytes", 0)}


def operation(event):
    owner = event.get("ownerUserId")
    cfg = m.config(owner)
    if not cfg:
        return r.response(403, {"error": "Owner recording access is not configured"})
    try:
        allowed = sources(cfg)
    except ValueError:
        return r.response(503, {"error": "Recording sources are not configured correctly"})
    source = event.get("source", "messages")
    if source not in allowed:
        return r.response(404, {"error": "Recording source not found"})
    if event.get("operation") == "owner_recordings_list":
        return listing(owner, source, allowed, event)
    if event.get("operation") != "owner_recording_media":
        return r.response(400, {"error": "Unknown recording operation"})
    sid = event.get("callSid", "")
    if not isinstance(sid, str) or not r.SID.fullmatch(sid) or event.get("disposition") not in {"inline", "attachment"}:
        return r.response(400, {"error": "Invalid recording request"})
    call = call_for(owner, source, sid)
    if not call:
        return r.response(404, {"error": "Recording not found"})
    if call.get("recordingStatus") != "ready":
        return r.response(409, {"error": "Recording is not ready", "recordingStatus": call.get("recordingStatus", "pending")})
    recording = call.get("recordingSid", "")
    expected = f"phone-trial/{call['runId']}/{sid}/{recording}.mp3"
    if not r.REC.fullmatch(recording) or call.get("s3Key") != expected:
        return r.response(409, {"error": "Recording storage binding is invalid"})
    s3 = r.clients()[1]
    try:
        head = s3.head_object(Bucket=os.environ["BUCKET"], Key=expected)
    except Exception as exc:
        code = getattr(exc, "response", {}).get("Error", {}).get("Code")
        if code in {"404", "NoSuchKey", "NotFound"}:
            return r.response(410, {"error": "Recording has expired or is no longer stored", "recordingStatus": "unavailable"})
        return r.response(503, {"error": "Recording storage cannot be verified"})
    if head.get("ContentLength", 0) != call.get("audioBytes") or head.get("ContentLength", 0) <= 0 or not re.fullmatch(r"[a-f0-9]{64}", head.get("Metadata", {}).get("sha256", "")):
        return r.response(409, {"error": "Recording integrity cannot be verified"})
    # Five-minute S3 links support browser Range/seek requests. Every fresh link
    # requires owner authentication; existing links retain this bounded lifetime.
    url = s3.generate_presigned_url("get_object", Params={"Bucket": os.environ["BUCKET"], "Key": expected,
        "ResponseContentType": "audio/mpeg", "ResponseContentDisposition": event["disposition"] + '; filename="' + sid + '.mp3"',
        "ResponseCacheControl": "private, no-store"}, ExpiresIn=TTL)
    return r.response(200, {"url": url, "expiresIn": TTL})


def listing(owner, source, allowed, event):
    limit = event.get("limit", 25)
    cursor = event.get("cursor", "")
    if type(limit) is not int or not 1 <= limit <= 50 or not isinstance(cursor, str):
        return r.response(400, {"error": "Invalid page parameters"})
    messages = source == "messages"
    prefix = "MESSAGE#" if messages else "CALL#"
    pk = "OWNER#" + owner if messages else "RUN#" + source
    if cursor and not (m.REQUEST_ID.fullmatch(cursor) if messages else r.SID.fullmatch(cursor)):
        return r.response(400, {"error": "Invalid page cursor"})
    args = {"KeyConditionExpression": "PK = :pk AND begins_with(SK, :prefix)",
            "ExpressionAttributeValues": {":pk": pk, ":prefix": prefix}, "ConsistentRead": True,
            "Limit": limit, "ScanIndexForward": False}
    if cursor:
        args["ExclusiveStartKey"] = {"PK": pk, "SK": prefix + cursor}
    page = r.clients()[0].query(**args)
    items = []
    for row in page.get("Items", []):
        if messages:
            if row.get("ownerUserId") != owner:
                continue
            call = call_for(owner, source, row["callSid"]) if row.get("callSid") else None
            item = projection(call) if call else {"recordingStatus": "pending" if row.get("callSid") else "absent"}
            item.update(requestId=row["requestId"], target=row.get("destination", ""),
                        providerStatus=row.get("providerStatus", "unknown"), updatedAt=row.get("updatedAt", row.get("createdAt", 0)))
        else:
            if row.get("runId") != source or not r.SID.fullmatch(row.get("callSid", "")):
                continue
            item = projection(row)
        items.append(item)
    result = {"items": items, "source": source, "sources": allowed}
    last = page.get("LastEvaluatedKey", {})
    if last.get("PK") == pk and last.get("SK", "").startswith(prefix):
        result["nextCursor"] = last["SK"][len(prefix):]
    return r.response(200, result)


def bind_legacy_recordings(main_table, phone_table, client):
    """CI deployment-only account binding; no caller or model supplies this run."""
    canonical = main_table.get_item(Key={"pk": "CONFIG", "sk": "OWNER"}, ConsistentRead=True).get("Item", {})
    cfg = phone_table.get_item(Key=m.CONFIG_KEY, ConsistentRead=True).get("Item", {})
    if not cfg and canonical.get("userId"):
        import account_lifecycle
        marker = phone_table.get_item(Key=account_lifecycle.owner_key(canonical["userId"]), ConsistentRead=True).get("Item", {})
        if marker.get("ownerUserId") == canonical["userId"] and marker.get("complete") is True:
            return
    owner = cfg.get("ownerUserId")
    if not owner or canonical.get("userId") != owner:
        raise ValueError("Historical recording owner binding could not be verified")
    run = "hotel-trial-2026-10-15"
    if "recordingRuns" in cfg:
        if run not in sources(cfg):
            raise ValueError("Historical recording source was not approved in existing configuration")
        return
    client.transact_write_items(TransactItems=[
        {"ConditionCheck": {"TableName": "live-ninja", "Key": {"pk": "CONFIG", "sk": "OWNER"},
                            "ConditionExpression": "userId=:owner", "ExpressionAttributeValues": {":owner": owner}}},
        {"Update": {"TableName": "live-ninja-phone-trial", "Key": m.CONFIG_KEY,
                    "UpdateExpression": "SET recordingRuns=:runs",
                    "ConditionExpression": "ownerUserId=:owner AND attribute_not_exists(recordingRuns)",
                    "ExpressionAttributeValues": {":owner": owner, ":runs": [run]}}}])
