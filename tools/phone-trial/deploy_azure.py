"""GitHub-only Azure deployment. Secrets stay in process memory and ARM stdin."""
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.request


def az(*args, value=None):
    result = subprocess.run(["az", *args, "--only-show-errors", "--output", "json"],
                            input=value, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            timeout=900, check=False)
    if result.returncode:
        raise RuntimeError("Azure command failed: " + " ".join(args[:3]))
    return json.loads(result.stdout) if result.stdout.strip() else {}


def main():
    if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("GITHUB_REF") != "refs/heads/main":
        raise SystemExit("Deploy only from GitHub Actions main.")
    cfg = json.loads(Path("infra/phone-trial-config.json").read_text())
    account = az("account", "show")
    if account["id"] != cfg["azureSubscriptionId"] or account["tenantId"] != cfg["azureTenantId"]:
        raise SystemExit("Wrong Azure subscription or tenant.")
    group, region = cfg["azureResourceGroup"], cfg["azureLocation"]
    registry = cfg["azureRegistryName"]
    az("acr", "create", "--name", registry, "--resource-group", group, "--location", region,
       "--sku", "Basic", "--admin-enabled", "true")
    tag = os.environ["GITHUB_SHA"]
    az("acr", "build", "--registry", registry, "--image", "phone-trial:" + tag,
       "--file", "containers/phone-trial-bridge/Dockerfile", "--no-logs", ".")
    environment = az("containerapp", "env", "create", "--name", cfg["azureEnvironmentName"],
                     "--resource-group", group, "--location", region, "--logs-destination", "none")
    domain = environment["properties"]["defaultDomain"]
    base = "https://" + cfg["azureAppName"] + "." + domain
    registry_info = az("acr", "show", "--name", registry, "--resource-group", group)
    registry_login = az("acr", "credential", "show", "--name", registry)
    targets = json.loads(Path("tools/phone-trial/targets.json").read_text())["hotels"]
    recorder = os.environ["RECORDER_URL"].rstrip("/")
    secrets = {"twilio-auth": os.environ["TWILIO_AUTH_TOKEN"],
               "twilio-api": os.environ["TWILIO_API_KEY_SECRET"],
               "azure-model": os.environ["AZURE_OPENAI_API_KEY"],
               "registry-password": registry_login["passwords"][0]["value"]}
    values = {"PORT": "8080", "PUBLIC_BASE_URL": base,
              "TWILIO_ACCOUNT_SID": os.environ["TWILIO_ACCOUNT_SID"],
              "TWILIO_API_KEY_SID": os.environ["TWILIO_API_KEY_SID"],
              "AZURE_OPENAI_ENDPOINT": cfg["modelEndpoint"], "AZURE_OPENAI_DEPLOYMENT": cfg["modelDeployment"],
              "RECORDING_CALLBACK_URL": recorder + "/recording", "EVENT_CALLBACK_URL": recorder + "/events",
              "RUN_ID": "hotel-trial-2026-10-15", "MAX_CALL_SECONDS": "180",
              "TARGETS_JSON": json.dumps([{"id": t["id"], "name": t["name"], "phone": t["phone"]} for t in targets])}
    env = [{"name": k, "value": v} for k, v in values.items()]
    env += [{"name": "TWILIO_AUTH_TOKEN", "secretRef": "twilio-auth"},
            {"name": "TWILIO_API_KEY_SECRET", "secretRef": "twilio-api"},
            {"name": "AZURE_OPENAI_API_KEY", "secretRef": "azure-model"}]
    body = {"name": cfg["azureAppName"], "type": "Microsoft.App/containerApps", "location": region,
            "properties": {"managedEnvironmentId": environment["id"],
                "configuration": {"activeRevisionsMode": "Single",
                    "ingress": {"external": True, "targetPort": 8080, "transport": "auto", "allowInsecure": False},
                    "secrets": [{"name": k, "value": v} for k, v in secrets.items()],
                    "registries": [{"server": registry_info["loginServer"], "username": registry_login["username"], "passwordSecretRef": "registry-password"}]},
                "template": {"containers": [{"name": "bridge", "image": registry_info["loginServer"] + "/phone-trial:" + tag,
                    "env": env, "resources": {"cpu": 0.25, "memory": "0.5Gi"},
                    "probes": [{"type": "Readiness", "httpGet": {"path": "/health", "port": 8080}, "initialDelaySeconds": 3, "periodSeconds": 10}]}],
                    "scale": {"minReplicas": 1, "maxReplicas": 1}}}}
    app = az("containerapp", "create", "--name", cfg["azureAppName"], "--resource-group", group,
             "--yaml", "/dev/stdin", value=json.dumps(body))
    actual = "https://" + app["properties"]["configuration"]["ingress"]["fqdn"]
    if actual != base:
        raise SystemExit("Unexpected Azure ingress URL; no calls started.")
    for attempt in range(30):
        try:
            with urllib.request.urlopen(base + "/health", timeout=10) as response:
                if response.status == 200 and json.load(response).get("ready") is True:
                    break
        except Exception:
            pass
        time.sleep(5)
    else:
        raise SystemExit("Bridge health check timed out.")
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as out:
        out.write("bridge_url=" + base + "\n")
    print("Azure phone bridge healthy; no calls started by deployment.")


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        raise SystemExit("Azure deployment failed: " + type(exc).__name__)
