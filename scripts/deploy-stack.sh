#!/usr/bin/env bash
# The two real callers are initial Lambda bootstrap and exact-ID ASK binding.
set -euo pipefail
if [[ "${GITHUB_REPOSITORY:-}" != "JeremyProffittOrg/live-ninja" || "${GITHUB_REF:-}" != "refs/heads/main" || "${GITHUB_ACTIONS:-}" != true ]]; then
  echo 'Deployment is restricted to this repository main GitHub Actions.' >&2
  exit 1
fi
config=$(python -B scripts/alexa-skill.py stack-config)
skill=$(python -c 'import json,sys; print(json.load(sys.stdin)["skillId"])' <<< "$config")
enabled=$(python -c 'import json,sys; print(json.load(sys.stdin)["enabled"])' <<< "$config")
sam deploy \
  --template-file template.yaml \
  --no-confirm-changeset \
  --no-fail-on-empty-changeset \
  --resolve-s3 \
  --parameter-overrides \
    "DomainName=$DOMAIN_NAME" \
    "CertificateArn=$CERTIFICATE_ARN" \
    "HostedZoneId=$HOSTED_ZONE_ID" \
    "OpenAIRealtimeModel=$OPENAI_REALTIME_MODEL" \
    "OpenAIMonthlyBudgetUsd=$OPENAI_MONTHLY_BUDGET_USD" \
    "NovaBridgeDesiredCount=$NOVA_DESIRED" \
    "NovaBridgeImageReady=$NOVA_IMAGE_READY" \
    NovaBridgeEnable=false \
    AgentCoreMemoryMode=all \
    "GhostCommandFunctionArn=$GHOST_FUNCTION" \
    "AlexaSkillId=$skill" \
    "AlexaEnabled=$enabled" \
    "AlexaTrialMode=${ALEXA_TRIAL_MODE:-true}" \
    "AlexaDeployCommit=$GITHUB_SHA"
