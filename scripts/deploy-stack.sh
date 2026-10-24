#!/usr/bin/env bash
# The two real callers are initial Lambda bootstrap and exact-ID ASK binding.
set -euo pipefail
if [[ "${GITHUB_REPOSITORY:-}" != "JeremyProffittOrg/live-ninja" || "${GITHUB_REF:-}" != "refs/heads/main" || "${GITHUB_ACTIONS:-}" != true ]]; then
  echo 'Deployment is restricted to this repository main GitHub Actions.' >&2
  exit 1
fi
if ! config=$(python -B scripts/alexa-skill.py stack-config); then
  # The helper emits only sanitized diagnostics; command substitution must
  # not hide the cause of a failed bootstrap from the deployment log.
  printf '%s\n' "$config" >&2
  exit 1
fi
skill=$(python -c 'import json,sys; print(json.load(sys.stdin)["skillId"])' <<< "$config")
enabled=$(python -c 'import json,sys; print(json.load(sys.stdin)["enabled"])' <<< "$config")
skill_override="AlexaSkillId=$skill"
if [[ -z "$skill" ]]; then
  # SAM needs literal quotes to represent an empty String override.
  skill_override='AlexaSkillId=""'
fi
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
    "$skill_override" \
    "AlexaEnabled=$enabled" \
    "AlexaTrialMode=${ALEXA_TRIAL_MODE:-false}" \
    "AlexaDeployCommit=$GITHUB_SHA"
