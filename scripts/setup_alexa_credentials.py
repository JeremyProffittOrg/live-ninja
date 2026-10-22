"""Local hidden-entry setup. No import effects, credential files, or CLI state copying."""
import argparse
import getpass
import hmac
import http.server
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

REPO = "JeremyProffittOrg/live-ninja"
ROOT = Path(__file__).resolve().parent.parent
FIELDS = (("variable", "ALEXA_SMAPI_CLIENT_ID"), ("secret", "ALEXA_SMAPI_CLIENT_SECRET"), ("secret", "ALEXA_SMAPI_REFRESH_TOKEN"))
CALLBACK = "http://127.0.0.1:9090/cb"


def command(args, value=None):
    result = subprocess.run(args, input=value, cwd=ROOT, capture_output=True, text=True,
                            encoding="utf-8", timeout=60, creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
    if result.returncode:
        raise RuntimeError("Required command failed (exit " + str(result.returncode) + "). Check local login/access; no value was printed.")
    return result.stdout


def configured():
    origin = command(["git", "remote", "get-url", "origin"]).strip().removesuffix(".git")
    if origin not in ("https://github.com/" + REPO, "git@github.com:" + REPO):
        raise RuntimeError("Wrong repository; nothing changed.")
    repo = json.loads(command(["gh", "repo", "view", REPO, "--json", "nameWithOwner,viewerPermission"]))
    if repo.get("nameWithOwner") != REPO or repo.get("viewerPermission") != "ADMIN":
        raise RuntimeError("GitHub administrator access to the exact repository is required.")
    return {kind: {x["name"] for x in json.loads(command(["gh", kind, "list", "--repo", REPO, "--json", "name"]))} for kind in ("variable", "secret")}


def read_value(name):
    value = getpass.getpass(name + " (hidden; Enter skips): ")
    print(str(len(value)) + " characters. Length confirms nonempty entry, not provider validity.")
    if any(c in value for c in "\r\n\x00"):
        raise RuntimeError("Single-line values only; nothing written for " + name)
    return value


def write_value(kind, name, value):
    if not value:
        print("SKIP: " + name + "; existing value unchanged.")
        return
    command(["gh", kind, "set", name, "--repo", REPO], value)
    names = json.loads(command(["gh", kind, "list", "--repo", REPO, "--json", "name"]))
    if name not in {x["name"] for x in names}:
        raise RuntimeError("Write completed but name verification failed for " + name + "; do not repeat blindly.")
    print("OK: " + name + " saved and its name verified.")


def authorize(client_id, client_secret):
    """User opens a fixed local URL. Codes/tokens stay in memory and out of logs."""
    state = secrets.token_urlsafe(32)
    result = {}
    authorize_url = "https://www.amazon.com/ap/oa?" + urllib.parse.urlencode({
        "client_id": client_id, "response_type": "code", "redirect_uri": CALLBACK, "state": state,
        "scope": "alexa::ask:skills:readwrite alexa::ask:models:readwrite alexa::ask:skills:test",
    })

    class Callback(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            parsed = urllib.parse.urlsplit(self.path)
            if parsed.path == "/":
                self.send_response(302)
                self.send_header("Location", "/start")
                self.end_headers()
                return
            if parsed.path == "/favicon.ico":
                self.send_response(204)
                self.end_headers()
                return
            if parsed.path == "/start":
                self.send_response(302)
                self.send_header("Location", authorize_url)
                self.send_header("Cache-Control", "no-store")
                self.end_headers()
                return
            fields = urllib.parse.parse_qs(parsed.query)
            valid = parsed.path == "/cb" and len(fields.get("state", [])) == 1 and hmac.compare_digest(fields["state"][0], state)
            if valid and len(fields.get("code", [])) == 1:
                result["code"] = fields["code"][0]
            self.send_response(200 if "code" in result else 400)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(b"Return to the setup window." if "code" in result else b"This authorization window is not current or Amazon declined it. Use the start address in the current setup window.")
            if valid and "code" not in result:
                code = fields.get("error", [""])[0]
                result["failed"] = code if code in {"access_denied", "invalid_scope", "invalid_request", "invalid_client", "unauthorized_client"} else "authorization_rejected"

    with http.server.HTTPServer(("127.0.0.1", 9090), Callback) as server:
        server.timeout = 1
        print("Open http://127.0.0.1:9090/start yourself. Sign in with the Jeramie LLC developer account and approve the requested publishing scopes.")
        deadline = time.monotonic() + 600
        while not result and time.monotonic() < deadline:
            server.handle_request()
    if not result.get("code"):
        if result.get("failed"):
            raise RuntimeError("Amazon authorization rejected: " + result["failed"] + ". No generated token was written.")
        raise RuntimeError("Amazon authorization was not completed within ten minutes. No generated token was written.")
    payload = urllib.parse.urlencode({"grant_type": "authorization_code", "code": result["code"],
                                     "redirect_uri": CALLBACK, "client_id": client_id, "client_secret": client_secret}).encode()
    try:
        with urllib.request.urlopen(urllib.request.Request("https://api.amazon.com/auth/o2/token", data=payload,
                                    headers={"Content-Type": "application/x-www-form-urlencoded"}), timeout=30) as response:
            granted = json.load(response)
            token = granted.get("refresh_token", "")
        with urllib.request.urlopen(urllib.request.Request("https://api.amazonalexa.com/v1/vendors",
                                    headers={"Authorization": "Bearer " + granted.get("access_token", "")}), timeout=30) as response:
            vendors = json.load(response).get("vendors", [])
        if not any(v.get("id") == "M3M80BVXTGPEZW" and "ROLE_ADMINISTRATOR" in v.get("roles", []) for v in vendors):
            raise RuntimeError("The authorized account does not administer the verified Jeramie LLC vendor. Nothing was written to GitHub.")
    except urllib.error.HTTPError as error:
        raise RuntimeError("Amazon token exchange failed (HTTP " + str(error.code) + "). No token value was printed.") from None
    if not token:
        raise RuntimeError("Amazon did not return a refresh token.")
    print("Generated refresh token: " + str(len(token)) + " characters; value stays hidden.")
    return token


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--authorize", action="store_true", help="Create a dedicated refresh token using your new LWA security profile, without printing it")
    args = parser.parse_args()
    names = configured()
    print("Repository: " + REPO + "; repository Actions scope, no GitHub environment.")
    print("Origin: https://live.jeremy.ninja")
    print("Privacy: https://live.jeremy.ninja/privacy")
    print("Login: https://live.jeremy.ninja/auth/lwa/login")
    print("Existing app callback: https://live.jeremy.ninja/auth/lwa/callback")
    print("Skill account linking: https://live.jeremy.ninja/oauth/authorize and https://live.jeremy.ninja/oauth/token")
    print("Amazon-generated skill callbacks will be discovered after the private skill is created; none is guessed here.")
    print("SMAPI security-profile setup: https://developer.amazon.com/en-US/docs/alexa/smapi/get-access-token-smapi.html")
    for kind, name in FIELDS:
        print(name + ": " + ("configured" if name in names[kind] else "missing"))
    if args.check:
        return 0 if all(name in names[kind] for kind, name in FIELDS) else 2
    if not sys.stdin.isatty():
        raise RuntimeError("Run this batch file in your own terminal for hidden entry. No values were requested.")
    with urllib.request.urlopen("https://live.jeremy.ninja/privacy", timeout=20) as page:
        if page.status != 200 or b"Privacy" not in page.read(100000):
            raise RuntimeError("The deployed privacy page could not be verified.")
    if args.authorize:
        if any(name in names[kind] for kind, name in FIELDS):
            raise RuntimeError("Some publishing values already exist. Use normal entry for missing values; this mode will not replace configured credentials.")
        print("Create a dedicated LWA security profile for Live Ninja publishing. In its Web Settings, add this exact Allowed Return URL: " + CALLBACK)
        client_id, client_secret = read_value(FIELDS[0][1]), read_value(FIELDS[1][1])
        if not client_id or not client_secret:
            print("SKIP: authorization was not started; all GitHub values remain unchanged.")
            return 2
        token = authorize(client_id, client_secret)
        for (kind, name), value in zip(FIELDS, (client_id, client_secret, token)):
            write_value(kind, name, value)
    else:
        for kind, name in FIELDS:
            if name in names[kind]:
                print("SKIP: " + name + " is already configured.")
            else:
                write_value(kind, name, read_value(name))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (RuntimeError, OSError, ValueError, subprocess.TimeoutExpired) as error:
        # Library exceptions can include URLs; never emit their payloads.
        print("STOP: " + (str(error) if isinstance(error, RuntimeError) else "Setup failed. No credential values were printed."))
        sys.exit(1)
