"""Private development skill delivery. AWS writes run only in this repo's main CI."""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile
import uuid
from datetime import datetime, timezone

ROOT = Path(__file__).resolve().parent.parent
REPO = "JeremyProffittOrg/live-ninja"
ACCOUNT = "759775734231"
VENDOR = "M3M80BVXTGPEZW"
BASE = "https://api.amazonalexa.com"
PREFIX = "/live-ninja/prod/alexa/"
SKILL_ID = re.compile(r"amzn1\.ask\.skill\.[A-Za-z0-9-]+\Z")


class Failure(RuntimeError):
    pass


class HTTPFailure(Failure):
    def __init__(self, status, code=""):
        self.status = status
        super().__init__("Amazon request failed: HTTP " + str(status) + (" " + code if code else ""))


class ImportFailed(Failure):
    pass


def command(args, stdin=None):
    result = subprocess.run(args, cwd=ROOT, input=stdin, text=True, encoding="utf-8", capture_output=True,
                            timeout=90, creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
    if result.returncode:
        # Provider/CLI stderr can include request payloads or signed URLs.
        code = re.search(r"\(([A-Za-z][A-Za-z0-9]+)\) when calling", result.stderr)
        detail = code.group(1) if code else next((label for text,label in (
            ("Unable to load paramfile", "CLI_INPUT_FILE"), ("Invalid JSON", "CLI_INPUT_JSON"),
            ("Error parsing parameter", "CLI_INPUT_PARSE"), ("Unknown options", "CLI_OPTION"),
            ("Unable to locate credentials", "CREDENTIALS_MISSING"), ("Could not connect", "ENDPOINT_UNAVAILABLE")) if text in result.stderr), "CLI_FAILURE")
        raise Failure("Required command failed: " + " ".join(args[:3]) + "; exit " + str(result.returncode) + "; " + detail)
    return json.loads(result.stdout) if result.stdout.strip() else {}


def aws(service, operation, **params):
    # Public request metadata uses a structured JSON argument. A secret Value
    # uses the CLI's scalar file parameter, read once from stdin. cli-input-json
    # can read its file more than once, so a pipe is not a valid JSON input file.
    args = ["aws", service, operation, "--region", "us-east-1", "--output", "json", "--no-cli-pager"]
    if os.name == "nt":
        if operation not in {"get-caller-identity", "get-parameters", "describe-stacks", "get-policy", "get-function-configuration", "get-item", "query", "filter-log-events"} or params.get("WithDecryption"):
            raise Failure("Only non-secret read operations are allowed through the local checker.")
    if service == "ssm" and operation == "put-parameter":
        return command(args+["--name", params["Name"], "--type", params["Type"], "--tier", params["Tier"], "--overwrite", "--value", "file:///dev/stdin"], params["Value"])
    return command(args+["--cli-input-json", json.dumps(params)])


def ci_only():
    if os.environ.get("GITHUB_REPOSITORY") != REPO or os.environ.get("GITHUB_REF") != "refs/heads/main" or os.environ.get("GITHUB_ACTIONS") != "true":
        raise Failure("Cloud writes require JeremyProffittOrg/live-ninja main GitHub Actions. No local deployment.")
    if aws("sts", "get-caller-identity").get("Account") != ACCOUNT:
        raise Failure("Wrong AWS account; no write performed.")


def parameter(name, decrypt=False):
    # GetParameters returns an explicit InvalidParameters list for absence;
    # access denied/network failures must not look like an empty configuration.
    result = aws("ssm", "get-parameters", Names=[PREFIX+name], WithDecryption=decrypt)
    values = result.get("Parameters", [])
    if not values:
        if result.get("InvalidParameters") == [PREFIX+name]:
            return None
        raise Failure("Cannot establish Alexa parameter state.")
    return values[0]["Value"]


def put_parameter(name, value, secret=False):
    ci_only()
    aws("ssm", "put-parameter", Name=PREFIX+name, Value=value, Type="SecureString" if secret else "String", Tier="Standard", Overwrite=True)


def request(url, method="GET", body=None, headers=None, raw=False):
    data = body if isinstance(body, bytes) else None if body is None else json.dumps(body).encode()
    hdr = {"Content-Type": "application/json", **(headers or {})}
    try:
        with urllib.request.urlopen(urllib.request.Request(url, data=data, headers=hdr, method=method), timeout=30) as response:
            content = response.read()
            return (content if raw else json.loads(content) if content else {}), dict(response.headers.items())
    except urllib.error.HTTPError as error:
        code = ""
        try:
            value = json.loads(error.read()).get("code", "")
            if re.fullmatch(r"[A-Za-z0-9_]{1,80}", str(value)):
                code = value
        except (ValueError, TypeError, AttributeError):
            pass
        raise HTTPFailure(error.code, code) from None
    except (urllib.error.URLError, TimeoutError):
        raise Failure("Amazon request outcome is uncertain; no mutation was retried.") from None


class SMAPI:
    def __init__(self):
        names = ("ALEXA_SMAPI_CLIENT_ID", "ALEXA_SMAPI_CLIENT_SECRET", "ALEXA_SMAPI_REFRESH_TOKEN")
        missing = [name for name in names if not os.environ.get(name)]
        self.local = len(missing) == len(names) and os.environ.get("GITHUB_ACTIONS") != "true"
        if self.local:
            self.token = ""
        elif missing:
            raise Failure("Missing CI configuration: " + ", ".join(missing))
        else:
            form = urllib.parse.urlencode({"grant_type": "refresh_token", "client_id": os.environ[names[0]],
                                          "client_secret": os.environ[names[1]], "refresh_token": os.environ[names[2]]}).encode()
            data, _ = request("https://api.amazon.com/auth/o2/token", "POST", form, {"Content-Type": "application/x-www-form-urlencoded"})
            self.token = data.get("access_token", "")
            if not self.token:
                raise Failure("Amazon returned no publishing access token.")
        vendors, _ = self.call("/v1/vendors")
        if not any(v.get("id") == VENDOR and "ROLE_ADMINISTRATOR" in v.get("roles", []) for v in vendors.get("vendors", [])):
            raise Failure("Publishing identity does not administer the verified Jeramie LLC vendor.")

    def call(self, path, method="GET", body=None, headers=None):
        parsed = urllib.parse.urlsplit(path)
        if parsed.scheme or parsed.netloc or not path.startswith("/v"):
            raise Failure("Unexpected SMAPI path.")
        if "/live/" in path or "/certifications" in path:
            raise Failure("Only private development changes are authorized.")
        if self.local:
            return self.local_call(path, method, body), {}
        for attempt in range(3 if method == "GET" else 1):
            try:
                return request(BASE+path, method, body, {"Authorization": "Bearer "+self.token, **(headers or {})})
            except HTTPFailure as error:
                if method != "GET" or error.status not in (429, 500, 502, 503, 504) or attempt == 2:
                    raise
                time.sleep(2**attempt)

    def local_call(self, path, method, body):
        # Use the installed CLI's existing machine-local login. Never open,
        # copy or export its credential configuration. Run node directly so
        # utterance punctuation never passes through cmd.exe interpolation.
        entry = Path(os.environ.get("APPDATA", ""))/"npm/node_modules/ask-cli/dist/bin/ask.js"
        node = shutil.which("node")
        if not node or not entry.is_file():
            raise Failure("Local verification requires the existing ASK CLI and Node installation.")
        if path == "/v1/vendors" and method == "GET":
            args = ["get-vendor-list"]
        elif match := re.fullmatch(r"/v1/skills/(amzn1\.ask\.skill\.[A-Za-z0-9-]+)/stages/development/(manifest|interactionModel/locales/en-US|accountLinkingClient)", path):
            if method != "GET":
                raise Failure("Local skill changes are forbidden; use GitHub delivery.")
            name = {"manifest": "get-skill-manifest", "interactionModel/locales/en-US": "get-interaction-model", "accountLinkingClient": "get-account-linking-info"}[match[2]]
            args = [name, "--skill-id", match[1], "--stage", "development"]
            if match[2].startswith("interactionModel"):
                args += ["--locale", "en-US"]
        elif match := re.fullmatch(r"/v2/skills/(amzn1\.ask\.skill\.[A-Za-z0-9-]+)/stages/development/simulations(?:/([A-Za-z0-9._-]+))?", path):
            args = ["get-skill-simulation" if match[2] else "simulate-skill", "--skill-id", match[1], "--stage", "development"]
            if match[2] and method == "GET":
                args += ["--simulation-id", match[2]]
            elif not match[2] and method == "POST":
                args += ["--input-content", body["input"]["content"], "--device-locale", "en-US", "--session-mode", body["session"]["mode"]]
            else:
                raise Failure("Invalid local simulation request.")
        else:
            raise Failure("This operation is not part of local Alexa verification.")
        return command([node, str(entry), "smapi"]+args+["--profile", "default"])


def skill_path(skill, suffix):
    if not SKILL_ID.fullmatch(skill or ""):
        raise Failure("Missing or invalid recorded Alexa skill ID.")
    return "/v1/skills/"+skill+"/stages/development/"+suffix


def local_files(endpoint=None):
    manifest = json.loads((ROOT/"alexa/skill.json").read_text(encoding="utf-8"))
    model = json.loads((ROOT/"alexa/interaction-models/en-US.json").read_text(encoding="utf-8"))
    core = model["interactionModel"]
    intents = {i["name"]: i for i in core["languageModel"]["intents"]}
    if core["languageModel"]["invocationName"] != "live ninja" or len(intents) != 10:
        raise Failure("Unexpected Alexa interaction contract.")
    chat = intents["ChatIntent"]
    if chat["slots"] != [{"name": "query", "type": "AMAZON.SearchQuery", "samples": ["{query}"]}] or any(s.strip() == "{query}" for s in chat["samples"]):
        raise Failure("SearchQuery needs the single query slot and carrier phrases.")
    if core["dialog"]["delegationStrategy"] != "SKILL_RESPONSE" or not core["dialog"]["intents"][0]["slots"][0]["elicitationRequired"]:
        raise Failure("Follow-up slot elicitation is missing.")
    if set(manifest["manifest"]["publishingInformation"]["locales"]) != {"en-US"} or manifest["manifest"].get("permissions"):
        raise Failure("Unexpected skill locale or permissions.")
    if endpoint:
        if not re.fullmatch(r"arn:aws:lambda:us-east-1:"+ACCOUNT+r":function:live-ninja-alexa-skill", endpoint):
            raise Failure("Endpoint must be this stack's actual Alexa Lambda ARN.")
        manifest["manifest"]["apis"]["custom"]["endpoint"] = {"uri": endpoint}
    return manifest, model


def package(manifest, model):
    content = io.BytesIO()
    with zipfile.ZipFile(content, "w", zipfile.ZIP_DEFLATED) as archive:
        archive.writestr("skill.json", json.dumps(manifest))
        archive.writestr("interactionModels/custom/en-US.json", json.dumps(model))
    return content.getvalue()


def listed_skills(api):
    found, token = {}, ""
    for _ in range(100):
        path = "/v1/skills?"+urllib.parse.urlencode({"vendorId": VENDOR, "maxResults": 50, **({"nextToken": token} if token else {})})
        data, _ = api.call(path)
        for item in data.get("skills", []):
            found.setdefault(item["skillId"], []).append(item)
        if not data.get("isTruncated"):
            return found
        token = data.get("nextToken", "")
        if not token:
            raise Failure("Truncated skill list omitted its next token.")
    raise Failure("Skill listing exceeded its page ceiling.")


def reconcile_skill(api, state, endpoint):
    candidates = []
    listing = listed_skills(api)
    for skill, rows in listing.items():
        if any(row.get("nameByLocale", {}).get("en-US") == "Live Ninja" for row in rows):
            data, _ = api.call(skill_path(skill, "manifest"))
            if data.get("manifest", {}).get("apis", {}).get("custom", {}).get("endpoint", {}).get("uri") == endpoint:
                if any(row.get("stage") != "development" or row.get("publicationStatus") != "DEVELOPMENT" for row in rows):
                    raise Failure("Live Ninja has a public/certification stage; private-development delivery will not adopt it.")
                candidates.append(skill)
    if state.get("skillId"):
        if state["skillId"] not in candidates:
            raise Failure("Recorded skill no longer matches the verified vendor, name and Lambda endpoint.")
        return state["skillId"]
    if len(candidates) == 1:
        return candidates[0]
    if candidates or state.get("createAttempted"):
        raise Failure("Skill creation needs reconciliation; no second skill will be created.")
    return None


def poll_import(api, path, timeout=900):
    if not re.fullmatch(r"/v1/skills/imports/[A-Za-z0-9._-]+", path or ""):
        raise Failure("Missing safe import-status location.")
    deadline = time.monotonic()+timeout
    while time.monotonic() < deadline:
        state, _ = api.call(path)
        status = state.get("status")
        if status == "SUCCEEDED":
            return state
        if status != "IN_PROGRESS":
            raise ImportFailed("Alexa package build ended with " + str(status) + "; inspect its import ID in the developer console.")
        time.sleep(5)
    raise Failure("Alexa package build exceeded fifteen minutes; its import ID is retained for reconciliation.")


def redirect_uris(vendor):
    if vendor != VENDOR:
        raise Failure("Unverified account-link vendor.")
    return [base+"/api/skill/link/"+vendor for base in ("https://pitangui.amazon.com", "https://layla.amazon.com", "https://alexa.amazon.co.jp")]


def configure_link(api, skill):
    raw = parameter("account-link-secret", True)
    if not raw and parameter("oauth-client"):
        raise Failure("The account-link secret is missing while its client exists. Restore the machine-independent SSM secret; automatic client replacement is forbidden.")
    credentials = json.loads(raw) if raw else {"clientId": secrets.token_urlsafe(32), "clientSecret": secrets.token_urlsafe(48)}
    if not raw:
        put_parameter("account-link-secret", json.dumps(credentials), True)
    if len(credentials.get("clientId", "")) != 43 or not credentials.get("clientSecret"):
        raise Failure("Stored account-link client is invalid; automatic replacement is forbidden.")
    config = {"clientId": credentials["clientId"], "secretHash": hashlib.sha256(credentials["clientSecret"].encode()).hexdigest(), "redirectUris": redirect_uris(VENDOR)}
    put_parameter("oauth-client", json.dumps(config))
    link = {"type": "AUTH_CODE", "skipOnEnablement": False, "authorizationUrl": "https://live.jeremy.ninja/oauth/authorize",
            "accessTokenUrl": "https://live.jeremy.ninja/oauth/token", "clientId": credentials["clientId"], "clientSecret": credentials["clientSecret"],
            "accessTokenScheme": "REQUEST_BODY_CREDENTIALS", "domains": ["live.jeremy.ninja"], "scopes": ["alexa:chat"],
            "defaultTokenExpirationInSeconds": 900, "pkceConfiguration": {"status": "ENABLED", "codeChallengeMethod": "S256"}}
    api.call(skill_path(skill, "accountLinkingClient"), "PUT", {"accountLinkingRequest": link})
    actual, _ = api.call(skill_path(skill, "accountLinkingClient"))
    for name in ("type", "skipOnEnablement", "authorizationUrl", "accessTokenUrl", "clientId", "accessTokenScheme", "scopes", "pkceConfiguration"):
        if actual.get(name) != link[name]:
            raise Failure("Account-link verification failed for " + name)


def publish(api):
    ci_only()
    stacks = aws("cloudformation", "describe-stacks", StackName="live-ninja")["Stacks"]
    outputs = {x["OutputKey"]: x["OutputValue"] for x in stacks[0].get("Outputs", [])}
    endpoint = outputs.get("AlexaSkillFunctionArn", "")
    manifest, model = local_files(endpoint)
    if not endpoint:
        raise Failure("Deploy the real Alexa Lambda before creating the skill.")
    state = json.loads(parameter("delivery-state") or "{}")
    if state.get("importUncertain"):
        raise Failure("The last skill package submission has an uncertain result. Reconcile its import ID in Amazon before another submission.")
    skill = reconcile_skill(api, state, endpoint)
    if not skill:
        state["createAttempted"] = True
        put_parameter("delivery-state", json.dumps(state))
        created, _ = api.call("/v1/skills", "POST", {"vendorId": VENDOR, **manifest})
        skill = created.get("skillId")
        skill_path(skill, "manifest")
    state["skillId"] = skill
    put_parameter("delivery-state", json.dumps(state))
    # Persist immediately; a failed package build must never create a new skill.
    put_parameter("skill-id", skill)
    if state.get("importPending"):
        try:
            poll_import(api, state["importPending"])
        except ImportFailed:
            if state.get("pendingCommit") == os.environ.get("GITHUB_SHA") or state.get("buildAttempts", 0) >= 3:
                raise
            state["lastFailedImport"] = state["importPending"]
        state.pop("importPending")
        put_parameter("delivery-state", json.dumps(state))
    upload, _ = api.call("/v1/skills/uploads", "POST")
    target = urllib.parse.urlsplit(upload.get("uploadUrl", ""))
    if target.scheme != "https" or not (target.hostname or "").endswith(".amazonaws.com") or target.username:
        raise Failure("Amazon returned an unexpected package upload host.")
    request(upload["uploadUrl"], "PUT", package(manifest, model), {"Content-Type": "application/zip"}, raw=True)
    if state.get("buildAttempts", 0) >= 3:
        raise Failure("Three corrected Alexa package attempts failed; reconcile before a new run.")
    state["buildAttempts"] = state.get("buildAttempts", 0)+1
    state["pendingCommit"] = os.environ.get("GITHUB_SHA")
    state["importUncertain"] = True
    put_parameter("delivery-state", json.dumps(state))
    _, headers = api.call("/v1/skills/"+skill+"/imports", "POST", {"location": upload["uploadUrl"]})
    location = next((v for k, v in headers.items() if k.lower() == "location"), "")
    state["importPending"] = location
    state["importUncertain"] = False
    put_parameter("delivery-state", json.dumps(state))
    imported = poll_import(api, location)
    state.update({"importId": location.rsplit("/", 1)[-1], "packageETag": imported.get("skill", {}).get("eTag"), "commit": os.environ.get("GITHUB_SHA", "")})
    state.pop("importPending")
    state["buildAttempts"] = 0
    put_parameter("delivery-state", json.dumps(state))
    configure_link(api, skill)
    return {"skillId": skill, "endpoint": endpoint, "stage": "development", "importId": state["importId"], "commit": state["commit"]}


def deployment(api):
    skill = parameter("skill-id")
    state = json.loads(parameter("delivery-state") or "{}")
    stacks = aws("cloudformation", "describe-stacks", StackName="live-ninja")["Stacks"]
    stack = stacks[0]
    if stack["StackStatus"] not in ("CREATE_COMPLETE", "UPDATE_COMPLETE"):
        raise Failure("Live Ninja stack is not successfully deployed.")
    outputs = {x["OutputKey"]: x["OutputValue"] for x in stack.get("Outputs", [])}
    endpoint = outputs.get("AlexaSkillFunctionArn")
    data, _ = api.call(skill_path(skill, "manifest"))
    if data.get("manifest", {}).get("apis", {}).get("custom", {}).get("endpoint", {}).get("uri") != endpoint:
        raise Failure("Skill and stack endpoints differ.")
    actual, _ = api.call(skill_path(skill, "interactionModel/locales/en-US"))
    _, expected = local_files(endpoint)
    if not contains_expected(actual.get("interactionModel"), expected["interactionModel"]):
        raise Failure("Deployed interaction model differs from this commit.")
    policy = json.loads(aws("lambda", "get-policy", FunctionName="live-ninja-alexa-skill")["Policy"])
    matching = [s for s in policy["Statement"] if s.get("Principal", {}).get("Service") == "alexa-appkit.amazon.com"]
    if len(matching) != 1 or matching[0].get("Condition", {}).get("StringEquals", {}).get("lambda:EventSourceToken") != skill:
        raise Failure("Alexa invoke permission is not restricted to the exact skill ID.")
    config = aws("lambda", "get-function-configuration", FunctionName="live-ninja-alexa-skill")
    worker = aws("lambda", "get-function-configuration", FunctionName="live-ninja-alexa-chat-worker")
    for function in (config, worker):
        if function.get("Environment", {}).get("Variables", {}).get("ALEXA_DEPLOYED_COMMIT") != state.get("commit") or not state.get("commit"):
            raise Failure("Alexa Lambda code configuration and skill package commit differ.")
    enabled = config.get("Environment", {}).get("Variables", {}).get("ALEXA_ENABLED") == "true"
    if not enabled and os.environ.get("ALEXA_ENABLED", "true") != "false":
        raise Failure("Alexa admissions are disabled.")
    return {"skillId": skill, "endpoint": endpoint, "stage": "development", "enabled": enabled, "importId": state.get("importId"), "commit": state.get("commit"), "stackStatus": stack["StackStatus"], "modelVersion": actual.get("version"), "workflowIds": [int(os.environ["GITHUB_RUN_ID"])] if os.environ.get("GITHUB_RUN_ID") else []}


def contains_expected(actual, expected):
    if isinstance(expected, dict):
        return isinstance(actual, dict) and all(k in actual and contains_expected(actual[k], v) for k, v in expected.items())
    if isinstance(expected, list):
        return isinstance(actual, list) and len(actual) == len(expected) and all(any(contains_expected(value, item) for value in actual) for item in expected)
    return actual == expected


def simulation_record(data, case):
    record = {"simulationId": data.get("id"), "expected": case["expected"], "status": data.get("status"), "passed": False, "outcome": "simulation_failed"}
    invocations = (data.get("result") or {}).get("skillExecutionInfo", {}).get("invocations", [])
    if data.get("status") != "SUCCESSFUL" or not invocations:
        return record
    invocation = invocations[-1]
    req = invocation.get("invocationRequest", {}).get("body", {})
    response = invocation.get("invocationResponse", {}).get("body", {})
    req = json.loads(req) if isinstance(req, str) else req
    response = json.loads(response) if isinstance(response, str) else response
    intent = req.get("request", {}).get("intent", {})
    name = intent.get("name", req.get("request", {}).get("type", ""))
    query = intent.get("slots", {}).get("query", {}).get("value", "").strip().lower().strip(" .!?")
    command_name = {"LaunchRequest": "launch", "ChatIntent": "chat", "StatusIntent": "status", "ResumeIntent": "resume", "MoreIntent": "more", "NewConversationIntent": "new", "AMAZON.StopIntent": "stop", "AMAZON.CancelIntent": "stop", "AMAZON.RepeatIntent": "repeat", "AMAZON.HelpIntent": "help"}.get(name, "unknown")
    if command_name == "chat" and query in {"status", "repeat", "more", "resume", "help", "stop", "cancel", "new conversation", "start over"}:
        command_name = {"cancel": "stop", "new conversation": "new", "start over": "new"}.get(query, query)
    speech = response.get("response", {}).get("outputSpeech", {}).get("text", "")
    linking = response.get("response", {}).get("card", {}).get("type") == "LinkAccount"
    pending = "still working" in speech.lower()
    failed = not speech or any(s in speech.lower() for s in ("did not finish", "timed out", "not enabled", "couldn't", "could not"))
    outcome = "account_link_required" if linking else "pending" if pending else "failure" if failed else "answer"
    elapsed = invocation.get("metrics", {}).get("skillExecutionTimeInMilliseconds")
    record.update({"requestId": req.get("request", {}).get("requestId"), "command": command_name, "skillMs": elapsed,
                   "outcome": outcome, "speech": speech, "passed": command_name == case["expected"] and not linking and not failed and isinstance(elapsed, (int, float)) and 0 <= elapsed < 8000})
    if case.get("answerPattern") and not re.search(case["answerPattern"], speech, re.IGNORECASE):
        record["passed"] = False
        record["answerMatched"] = False
    elif case.get("answerPattern"):
        record["answerMatched"] = True
    # Only derived references, never raw Alexa identities or linked tokens.
    owner = case.get("owner")
    provider_device = req.get("context", {}).get("System", {}).get("device", {}).get("deviceId")
    if owner and provider_device and record.get("requestId"):
        device = str(uuid.uuid5(uuid.NAMESPACE_URL, "live-ninja/alexa/"+owner+"/"+case["skillId"]+"/"+provider_device))
        record["receipt"] = hashlib.sha256((device+"\0"+record["requestId"]).encode()).hexdigest()
    return record


def trial_usage(owner, table):
    data = aws("dynamodb", "query", TableName=table, ConsistentRead=True, KeyConditionExpression="pk = :owner AND begins_with(sk, :prefix)",
               ExpressionAttributeValues={":owner": {"S": "USER#"+owner}, ":prefix": {"S": "ALEXAUSAGE#"}})
    total = 0.0
    for row in data.get("Items", []):
        if not row.get("reported", {}).get("BOOL"):
            raise Failure("An Alexa provider request has unreported usage; reconcile it before spending more of the $1 trial allowance.")
        total += float(row["estimatedCostUSD"]["N"])
    return total


def run_simulation(api, skill, case, owner):
    path = "/v2/skills/"+skill+"/stages/development/simulations"
    data, _ = api.call(path, "POST", {"session": {"mode": "FORCE_NEW_SESSION" if case.get("newSession") else "DEFAULT"}, "input": {"content": case["utterance"]}, "device": {"locale": "en-US"}})
    identifier = data.get("id", "")
    if not re.fullmatch(r"[A-Za-z0-9._-]+", identifier):
        raise Failure("Simulation returned no safe provider ID; no retry was submitted.")
    deadline = time.monotonic()+60
    while data.get("status") == "IN_PROGRESS" and time.monotonic() < deadline:
        time.sleep(1)
        data, _ = api.call(path+"/"+identifier)
    record = simulation_record(data, {**case, "owner": owner, "skillId": skill})
    record["utterance"] = case["utterance"]
    return record


def simulate(api, mode, evidence):
    deploy = deployment(api)
    skill = deploy["skillId"]
    worker = aws("lambda", "get-function-configuration", FunctionName="live-ninja-alexa-chat-worker")
    if worker.get("Environment", {}).get("Variables", {}).get("ALEXA_TRIAL_MODE") != "true":
        raise Failure("Initial acceptance requires the CI-deployed conversation-only trial guard; no paid test was started.")
    stack = aws("cloudformation", "describe-stacks", StackName="live-ninja")["Stacks"][0]
    table = next(x["OutputValue"] for x in stack["Outputs"] if x["OutputKey"] == "TableName")
    owner = aws("dynamodb", "get-item", TableName=table, ConsistentRead=True, Key={"pk": {"S": "CONFIG"}, "sk": {"S": "OWNER"}}).get("Item", {}).get("userId", {}).get("S")
    if not owner:
        raise Failure("Active owner binding is unavailable.")
    cases = json.loads((ROOT/"alexa/simulations.json").read_text(encoding="utf-8"))
    if mode == "latency":
        words = ["two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven"]
        cases = [{"utterance": "open live ninja", "expected": "launch", "newSession": True}] + [{"utterance": "What is "+str(i%10+1)+" plus one?", "expected": "chat", "answerPattern": r"\b("+str(i%10+2)+"|"+words[i%10]+r")\b"} for i in range(100)]
    report = {"kind": mode, "commit": deploy["commit"], "skillId": skill, "startedAt": datetime.now(timezone.utc).isoformat(), "cases": []}
    last_admission = 0.0
    for case in cases:
        if (datetime.now(timezone.utc)-datetime.fromisoformat(report["startedAt"])).total_seconds() > 3600:
            raise Failure("Alexa acceptance reached its one-hour run ceiling. Evidence is retained.")
        # Preserve the existing account rate limit; this is conversational
        # latency acceptance, not an unapproved load/burst test.
        time.sleep(max(0, 5-(time.monotonic()-last_admission)))
        if trial_usage(owner, table) >= .93:
            raise Failure("The $1 Alexa trial has less than the conservative five-round $0.07 allowance remaining; no new model request was started.")
        last_admission = time.monotonic()
        record = run_simulation(api, skill, case, owner)
        report["cases"].append(record)
        report["estimatedTrialCostUSD"] = trial_usage(owner, table)
        write_evidence(evidence, report)
        print(json.dumps({"case": len(report["cases"]), "kind": mode, "outcome": record["outcome"], "skillMs": record.get("skillMs"), "estimatedTrialCostUSD": report["estimatedTrialCostUSD"]}), flush=True)
        if record["outcome"] == "account_link_required":
            raise Failure("Link the development skill to Live Ninja using the same Alexa developer account before testing.")
        if record["outcome"] == "pending":
            # Wait for the admitted receipt. Never resubmit the question.
            time.sleep(25)
            retrieved = run_simulation(api, skill, {"utterance": "status", "expected": "status"}, owner)
            record["retrieval"] = retrieved
            if retrieved["outcome"] != "answer" or not retrieved["passed"]:
                write_evidence(evidence, report)
                raise Failure("The pending answer did not become retrievable. Its original question was not resubmitted.")
        if record["outcome"] in {"simulation_failed", "failure"}:
            raise Failure("Alexa test produced an explicit failure; its sanitized evidence was saved.")
    rows = report["cases"]
    passed = sum(row["passed"] for row in rows)/len(rows)
    normal = [row for row in rows if row["expected"] == "chat"]
    times = sorted(row["skillMs"] for row in normal if row.get("skillMs") is not None)
    p95 = times[min(len(times)-1, int(.95*len(times)))] if times else None
    report.update({"endedAt": datetime.now(timezone.utc).isoformat(), "routingRate": passed, "ordinaryReadyRate": sum(row["outcome"] == "answer" for row in normal)/max(1,len(normal)), "normalP95Ms": p95})
    # Runtime logs contain numeric timing plus opaque receipts, not transcripts.
    events = aws("logs", "filter-log-events", LogGroupName="/live-ninja/lambda/alexa-chat-worker", StartTime=int(datetime.fromisoformat(report["startedAt"]).timestamp()*1000), FilterPattern='"Alexa"').get("events", [])
    timings = {}
    for event in events:
        try:
            item = json.loads(event["message"])
            if item.get("receipt"):
                entry = timings.setdefault(item["receipt"], {"modelMs": 0})
                if "queueWaitMs" in item: entry["queueWaitMs"] = item["queueWaitMs"]
                if "modelMs" in item: entry["modelMs"] += item["modelMs"]
        except (ValueError, KeyError, TypeError):
            continue
    for row in rows:
        row.update(timings.get(row.get("receipt"), {}))
    report["passed"] = passed >= .95 and all(isinstance(row.get("skillMs"),(int,float)) and row["skillMs"] < 8000 for row in rows) and (mode != "latency" or len(normal) == 100 and p95 is not None and p95 < 6000 and report["ordinaryReadyRate"] >= .95 and all("queueWaitMs" in row and "modelMs" in row for row in normal))
    write_evidence(evidence, report)
    if not report["passed"]:
        raise Failure("Alexa routing or latency gate failed; use the saved provider IDs to diagnose it.")
    return {k:v for k,v in report.items() if k != "cases"}


def validate_acceptance(simulations, latency, physical):
    if len(simulations.get("cases", [])) < 50 or simulations.get("routingRate", 0) < .95 or not simulations.get("passed"):
        raise Failure("Fifty successful provider routing cases are required.")
    if len([r for r in latency.get("cases", []) if r.get("expected") == "chat"]) != 100 or not latency.get("passed"):
        raise Failure("The 100-turn latency gate is incomplete.")
    for report in (simulations, latency):
        if not report.get("skillId") or any(not r.get("simulationId") or not r.get("requestId") or not isinstance(r.get("skillMs"),(int,float)) for r in report["cases"]):
            raise Failure("Actual provider IDs and timings are missing.")
    required = {"follow-up", "persona-profile", "pause-five", "pause-ten", "pause-thirty", "stop", "resume", "privacy-off", "revocation", "slow-result"}
    observations = physical.get("observations", [])
    if not required <= {x.get("case") for x in observations} or any(not x.get("utterance") or not x.get("heard") or not x.get("receipt") for x in observations):
        raise Failure("Real Echo observations with utterance, heard response and matching receipt are required; passed=true is not physical evidence.")
    started = datetime.fromisoformat(physical.get("startedAt", ""))
    ended = datetime.fromisoformat(physical.get("endedAt", ""))
    if (ended-started).total_seconds() < 300 or physical.get("skillId") != simulations["skillId"] or not physical.get("deviceId") or simulations.get("commit") != latency.get("commit"):
        raise Failure("Physical test duration, skill ID or tested commit does not match.")
    return {"passed": True, "skillId": simulations["skillId"], "commit": simulations["commit"], "physicalDurationSeconds": (ended-started).total_seconds()}


def verify_echo_receipts(physical):
    stack = aws("cloudformation", "describe-stacks", StackName="live-ninja")["Stacks"][0]
    table = next(x["OutputValue"] for x in stack["Outputs"] if x["OutputKey"] == "TableName")
    owner = aws("dynamodb", "get-item", TableName=table, ConsistentRead=True, Key={"pk": {"S": "CONFIG"}, "sk": {"S": "OWNER"}}).get("Item", {}).get("userId", {}).get("S")
    device = aws("dynamodb", "get-item", TableName=table, ConsistentRead=True, Key={"pk": {"S": "DEVICE#"+physical["deviceId"]}, "sk": {"S": "META"}}).get("Item", {})
    if device.get("surface", {}).get("S") != "alexa" or device.get("userId", {}).get("S") != owner:
        raise Failure("Physical evidence does not name an Alexa device registered to the active owner.")
    start = datetime.fromisoformat(physical["startedAt"]).timestamp()*1000
    end = datetime.fromisoformat(physical["endedAt"]).timestamp()*1000
    receipts = {o["receipt"] for o in physical["observations"]}
    if len(receipts) < 5:
        raise Failure("Physical evidence needs at least five distinct actual Echo turns.")
    for receipt in receipts:
        if not re.fullmatch(r"[0-9a-f]{64}", receipt):
            raise Failure("Invalid physical turn receipt.")
        row = aws("dynamodb", "get-item", TableName=table, ConsistentRead=True, Key={"pk": {"S": "USER#"+owner}, "sk": {"S": "ALEXA#TURN#"+receipt}}).get("Item", {})
        turn = json.loads(row.get("data", {}).get("S", "{}"))
        if turn.get("Identity", {}).get("Device") != physical["deviceId"] or not start <= turn.get("Deadline", 0)-30000 <= end:
            raise Failure("Physical receipt does not match this Echo and observation window. Capture evidence before its expiry cleanup.")


def write_evidence(path, value):
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(value, indent=2)+"\n", encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["validate-local", "preflight", "publish", "verify-deployment", "stack-config", "simulate", "latency", "acceptance", "delivery"])
    parser.add_argument("--evidence")
    args = parser.parse_args()
    local_files()
    result = {"localContract": "passed"}
    if args.action == "stack-config":
        ci_only()
        skill = parameter("skill-id") or ""
        if skill:
            skill_path(skill, "manifest")
        # Absence means disabled bootstrap. Failure to READ never means absent.
        result = {"skillId": skill, "enabled": "true" if os.environ.get("ALEXA_ENABLED", "true") == "true" and skill and parameter("oauth-client") else "false"}
        path = os.environ.get("GITHUB_OUTPUT")
        if not path:
            raise Failure("stack-config requires GitHub output context.")
        with open(path, "a", encoding="utf-8") as output:
            for name, value in result.items():
                output.write(name+"="+value+"\n")
    elif args.action != "validate-local":
        api = SMAPI()
        if args.action == "preflight":
            if api.local:
                import setup_alexa_credentials as setup
                names = setup.configured()
                missing = [name for kind, name in setup.FIELDS if name not in names[kind]]
                if missing:
                    raise Failure("Local ASK login works; GitHub still needs " + ", ".join(missing))
                result = {"vendorId": VENDOR, "localASKAccess": "verified", "ciCredentialNames": "present", "ciCredentialValidity": "verified only by the GitHub publish step"}
            else:
                result = {"vendorId": VENDOR, "publishingAccess": "verified"}
        elif args.action == "publish":
            result = publish(api)
        elif args.action in {"simulate", "latency"}:
            if not args.evidence:
                raise Failure("A sanitized evidence output path is required for paid test runs.")
            result = simulate(api, args.action, args.evidence)
            print(json.dumps(result))
            return 0
        elif args.action == "acceptance":
            root = Path(args.evidence or "artifacts/alexa")
            evidence = [json.loads((root/name).read_text(encoding="utf-8")) for name in ("simulations.json", "latency.json", "echo-observations.json")]
            result = validate_acceptance(*evidence)
            verify_echo_receipts(evidence[2])
            verified = deployment(api)
            if verified["commit"] != result["commit"]:
                raise Failure("Acceptance evidence is from a different deployment.")
            write_evidence(root/"acceptance.json", result)
            print(json.dumps(result))
            return 0
        elif args.action == "delivery":
            if not args.evidence:
                raise Failure("Provide the recorded deployment evidence JSON path.")
            recorded = json.loads(Path(args.evidence).read_text(encoding="utf-8"))
            for run in recorded.get("workflowIds", []):
                info = command(["gh", "run", "view", str(run), "--repo", REPO, "--json", "status,conclusion,headSha"])
                if info != {"status": "completed", "conclusion": "success", "headSha": recorded.get("commit")}:
                    raise Failure("Recorded deployment workflow is not successful for this commit.")
            if not recorded.get("workflowIds"):
                raise Failure("Deployment evidence has no workflow IDs.")
            result = deployment(api)
            if result["commit"] != recorded.get("commit"):
                raise Failure("Skill package and workflow commit differ.")
        else:
            result = deployment(api)
    if args.evidence:
        write_evidence(args.evidence, result)
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (Failure, ValueError, KeyError, OSError, subprocess.TimeoutExpired) as error:
        print("BLOCKED: " + (str(error) if isinstance(error, Failure) else "Alexa delivery failed; credential values and provider payloads were not printed."))
        sys.exit(1)
