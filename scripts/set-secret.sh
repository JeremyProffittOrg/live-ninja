#!/usr/bin/env bash
# set-secret.sh — the ONLY sanctioned way to put a secret on this repo.
#
# Agents: run this and let the USER type the value into the hidden prompt.
# The value never appears in chat, argv, logs, or files (unless --file is used,
# in which case the file should be deleted afterward).
#
#   ./scripts/set-secret.sh SECRET_NAME              # hidden interactive prompt
#   ./scripts/set-secret.sh SECRET_NAME --file PATH  # multiline (private keys, certs)
#   ./scripts/set-secret.sh SECRET_NAME --generate   # random 48-byte base64 (JWT/session/HMAC)
#
# Requires: gh (authenticated), git remote 'origin' pointing at the GitHub repo.

set -euo pipefail
export MSYS_NO_PATHCONV=1

NAME="${1:-}"
[ -n "$NAME" ] || { echo "usage: $0 SECRET_NAME [--file PATH | --generate]" >&2; exit 2; }
[[ "$NAME" =~ ^[A-Z][A-Z0-9_]*$ ]] || { echo "ERROR: secret names are UPPER_SNAKE_CASE" >&2; exit 2; }
MODE="${2:-}"

command -v gh >/dev/null || { echo "ERROR: gh CLI not found" >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "ERROR: gh not authenticated (run: gh auth login)" >&2; exit 1; }

REPO="$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null)" \
  || { echo "ERROR: cannot resolve repo from origin remote" >&2; exit 1; }

case "$MODE" in
  --file)
    FILE="${3:-}"
    [ -s "$FILE" ] || { echo "ERROR: file '$FILE' missing or empty" >&2; exit 1; }
    VALUE="$(cat "$FILE")"
    ;;
  --generate)
    VALUE="$(openssl rand -base64 48)"
    ;;
  "")
    [ -t 0 ] || { echo "ERROR: interactive prompt needs a terminal (agents: run this in the user's terminal, do NOT pipe a value in)" >&2; exit 1; }
    read -r -s -p "Enter value for $NAME (hidden): " VALUE </dev/tty; echo >&2
    read -r -s -p "Confirm: " VALUE2 </dev/tty; echo >&2
    [ "$VALUE" = "$VALUE2" ] || { echo "ERROR: values did not match" >&2; exit 1; }
    ;;
  *) echo "ERROR: unknown option '$MODE'" >&2; exit 2 ;;
esac
[ -n "$VALUE" ] || { echo "ERROR: refusing to set an empty value" >&2; exit 1; }

printf '%s' "$VALUE" | gh secret set "$NAME" -R "$REPO"
FP="$(printf '%s' "$VALUE" | openssl dgst -sha256 | awk '{print $2}' | cut -c1-12)"
LEN="$(printf '%s' "$VALUE" | wc -c | tr -d ' ')"
VALUE=""; VALUE2=""

UPDATED="$(gh api "repos/$REPO/actions/secrets/$NAME" --jq .updated_at)"
echo "OK: $NAME set on $REPO (len $LEN, sha256:$FP..., updated_at $UPDATED)"
echo "Reminder: secrets reach running stacks only on the next deploy (push to main)."
