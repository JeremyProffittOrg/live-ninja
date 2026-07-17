# M0 Setup Checklist

One-time manual actions required before launch. All GitHub secrets and SSM parameters are synced automatically by the deploy workflow.

---

## [ ] Cost Allocation Tags — Activate in Billing Console

Non-retroactive activation required for Cost Explorer cost tracking to work retroactively from first deployment.

**AWS Console path:**
1. AWS Billing console → **Cost Management** → **Cost Allocation Tags** → **AWS-Generated Tags** tab
2. Find tags `Project` and `CostCenter`
3. Click **Activate** for each

**Alternative — CLI:**
```bash
aws ce update-cost-allocation-tags-status \
  --tags-to-activate "Project" "CostCenter"
```

Once activated, costs tagged with `Project=live-ninja` and `CostCenter=voice-ai` will be breakable in Cost Explorer.

---

## [ ] SNS Ops Topic — Email Subscription

Alarms route to SNS topic `OpsTopic` with email subscription `proffitt.jeremy@gmail.com`. Confirm the subscription:

1. Check email inbox (and spam folder) for AWS SNS Subscription Confirmation
2. Click the confirmation link in the email
3. Subscription status should show `Confirmed` in SNS console

If email not received or lost:
- AWS Console → SNS → Topics → select the `live-ninja-OpsTopic-*` → **Subscriptions** → **Request confirmation** on the pending subscription

---

## [ ] SES — Domain Verification & Production Access

**DKIM verification status for jeremy.ninja:** Already verified (DKIM enabled in global CLAUDE.md).

**Production access request (needed for launch):**
- The `EmailDispatchFunction` sends mail via SES from `Jeremy Proffitt <jeremy@jeremy.ninja>` with Reply-To `proffitt.jeremy@gmail.com`
- AWS SES in `us-east-1` requires explicit production access request (sandbox mode limits recipients)
- File the request at AWS Console → SES → **Account dashboard** → **Request production access**
  - Explain: "Low-volume transactional email service for live-ninja voice AI application"
  - Include domain jeremy.ninja and reply-to proffitt.jeremy@gmail.com
  - Typical approval: same day

Do NOT launch (M8) until production access is granted.

---

## [ ] Bedrock Nova Sonic Model Access — us-east-1

Needed only for M12 (RealtimeBrokerFunction calls Bedrock). Request in advance to avoid delays.

**AWS Console path:**
1. AWS Console → **Bedrock** → **Model access** (left nav)
2. Region: **us-east-1**
3. Find **Nova Sonic** in the list
4. Click **Manage model access** → **Request access**

Models typically available within hours. Once granted, `us-east-1` will show "Access granted" for Nova Sonic.

---

## [ ] GitHub Secrets & SSM Parameters

✓ **Already automated** — GitHub Actions deploy workflow (`deploy.yml`) syncs secrets and vars to AWS SSM Parameter Store on each deploy:
- `OPENAI_API_KEY` (secret) → `/live-ninja/prod/openai/api_key` (SecureString)
- `LWA_CLIENT_ID` (var) → `/live-ninja/prod/lwa/client_id` (String)
- `LWA_CLIENT_SECRET` (secret) → `/live-ninja/prod/lwa/client_secret` (SecureString)
- `DEVICE_CRED_PEPPER` (secret) → `/live-ninja/prod/device/cred_pepper` (SecureString, generated if missing)

No manual SSM setup needed.

---

**Status:** Proceed to first deploy once items 1–3 are complete (item 4 can be deferred until M12 planning).
