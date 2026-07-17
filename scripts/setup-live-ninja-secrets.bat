@echo off
REM ============================================================================
REM  setup-live-ninja-secrets.bat
REM  Captures the credentials Live Ninja needs and pushes them to GitHub.
REM
REM  Policy (see deploy.md):
REM   - AWS auth is OIDC only. There are NO AWS access keys to store here; the
REM     deploy role ARN is already the repo variable AWS_DEPLOY_ROLE_ARN.
REM   - Secret VALUES are typed by YOU into a hidden prompt and pushed straight to
REM     GitHub via gh. This script (and the agent) never see or log them.
REM   - Non-secret config (client IDs, URLs) go to GitHub *variables*, not secrets.
REM
REM  The deploy workflow copies the GitHub secrets into SSM Parameter Store
REM  (SecureString) at deploy time; the Go Lambdas read them from SSM at runtime.
REM ============================================================================
setlocal
cd /d "%~dp0.."

echo(
echo === Live Ninja credential setup ===
echo(

where gh >nul 2>&1
if errorlevel 1 (
  echo [X] GitHub CLI ^(gh^) not found on PATH. Install it, then re-run.
  exit /b 1
)
gh auth status >nul 2>&1
if errorlevel 1 (
  echo [X] Not logged in to GitHub CLI. Run:  gh auth login
  exit /b 1
)
for /f "delims=" %%r in ('gh repo view --json nameWithOwner -q .nameWithOwner 2^>nul') do set "REPO=%%r"
echo Target repo: %REPO%
echo(

REM ---------------------------------------------------------------------------
REM  0) Configure Login with Amazon FIRST (URLs to register), then continue
REM ---------------------------------------------------------------------------
echo ============================================================
echo   STEP 1 of 2 - Configure your Login with Amazon profile
echo ============================================================
echo   Open: https://developer.amazon.com/settings/console/securityprofile
echo   Select your Live Ninja Security Profile, open "Web Settings", and set:
echo(
echo   Allowed Origins:
echo     https://live.jeremy.ninja
echo(
echo   Allowed Return URLs:
echo     https://live.jeremy.ninja/auth/lwa/callback          [web]
echo     https://live.jeremy.ninja/auth/lwa/android           [Android app-link]
echo     https://live.jeremy.ninja/auth/lwa/device/callback   [M5Stack device setup]
echo     http://localhost:8080/auth/lwa/callback              [local dev - optional]
echo(
echo   Consent Privacy Notice URL:
echo     https://live.jeremy.ninja/privacy
echo(
echo   All surfaces use the backend BFF, so every return URL stays on
echo   live.jeremy.ninja. Then copy the Client ID + Client Secret from that
echo   same Security Profile - you will paste them below.
echo ============================================================
pause
echo(

REM ---------------------------------------------------------------------------
REM  1) NON-SECRET configuration  ->  GitHub VARIABLES
REM     (safe to echo; these are public identifiers)
REM ---------------------------------------------------------------------------
echo --- Non-secret config (GitHub variables) ---
set "LWA_CLIENT_ID="
set /p "LWA_CLIENT_ID=Login with Amazon Client ID (amzn1.application-oa2-client....) [enter to skip]: "
if not "%LWA_CLIENT_ID%"=="" (
  gh variable set LWA_CLIENT_ID --body "%LWA_CLIENT_ID%" && echo   [ok] variable LWA_CLIENT_ID set
)

set "LWA_RETURN_URL="
set /p "LWA_RETURN_URL=LWA web return URL [enter for default https://live.jeremy.ninja/auth/lwa/callback]: "
if "%LWA_RETURN_URL%"=="" set "LWA_RETURN_URL=https://live.jeremy.ninja/auth/lwa/callback"
gh variable set LWA_RETURN_URL --body "%LWA_RETURN_URL%" && echo   [ok] variable LWA_RETURN_URL set

REM Realtime model is fixed to the default (no prompt - avoids anyone pasting a key here).
set "OPENAI_REALTIME_MODEL=gpt-realtime"
gh variable set OPENAI_REALTIME_MODEL --body "%OPENAI_REALTIME_MODEL%" && echo   [ok] variable OPENAI_REALTIME_MODEL=gpt-realtime set

set "OPENAI_MONTHLY_BUDGET_USD="
set /p "OPENAI_MONTHLY_BUDGET_USD=OpenAI monthly spend cap in USD (metering/quota gate) [enter for default: 100]: "
if "%OPENAI_MONTHLY_BUDGET_USD%"=="" set "OPENAI_MONTHLY_BUDGET_USD=100"
gh variable set OPENAI_MONTHLY_BUDGET_USD --body "%OPENAI_MONTHLY_BUDGET_USD%" && echo   [ok] variable OPENAI_MONTHLY_BUDGET_USD set

echo(
REM ---------------------------------------------------------------------------
REM  2) SECRETS  ->  GitHub SECRETS  (HIDDEN input via set-secret.cmd)
REM     You will be prompted; input is not echoed. Values never reach the agent.
REM ---------------------------------------------------------------------------
echo --- Required secrets (value goes straight to GitHub; gh does not echo it) ---
echo(
echo   OPENAI_API_KEY - paste your OpenAI API key at the prompt below:
gh secret set OPENAI_API_KEY
if errorlevel 1 (echo   [X] OPENAI_API_KEY was NOT set - re-run and paste the key when prompted.) else (echo   [ok] OPENAI_API_KEY set)
echo(
echo   LWA_CLIENT_SECRET - paste your Login with Amazon client secret at the prompt below:
gh secret set LWA_CLIENT_SECRET
if errorlevel 1 (echo   [X] LWA_CLIENT_SECRET was NOT set - re-run and paste the secret when prompted.) else (echo   [ok] LWA_CLIENT_SECRET set)
echo(

echo --- Optional secret ---
echo   PICOVOICE_ACCESS_KEY is ONLY needed if you use Porcupine for custom wake words.
echo   The default programmable wake-word engine is openWakeWord (open-source, no key).
choice /c YN /m "Set PICOVOICE_ACCESS_KEY now"
if errorlevel 2 goto after_pico
gh secret set PICOVOICE_ACCESS_KEY
:after_pico

echo(
echo === Done. Current secret + variable names on %REPO% (names only): ===
echo(
echo -- secrets --
gh secret list
echo(
echo -- variables --
gh variable list
echo(
echo Next: push to main to trigger the OIDC deploy pipeline. The workflow syncs
echo these GitHub secrets into SSM Parameter Store (SecureString) for the Lambdas.
endlocal
