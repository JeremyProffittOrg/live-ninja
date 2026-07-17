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
set /p "LWA_RETURN_URL=LWA Allowed Return URL (e.g. https://live.jeremy.ninja/auth/callback) [enter to skip]: "
if not "%LWA_RETURN_URL%"=="" (
  gh variable set LWA_RETURN_URL --body "%LWA_RETURN_URL%" && echo   [ok] variable LWA_RETURN_URL set
)

set "OPENAI_REALTIME_MODEL="
set /p "OPENAI_REALTIME_MODEL=OpenAI Realtime model [enter for default: gpt-realtime]: "
if "%OPENAI_REALTIME_MODEL%"=="" set "OPENAI_REALTIME_MODEL=gpt-realtime"
gh variable set OPENAI_REALTIME_MODEL --body "%OPENAI_REALTIME_MODEL%" && echo   [ok] variable OPENAI_REALTIME_MODEL set

set "OPENAI_MONTHLY_BUDGET_USD="
set /p "OPENAI_MONTHLY_BUDGET_USD=OpenAI monthly spend cap in USD (metering/quota gate) [enter for default: 100]: "
if "%OPENAI_MONTHLY_BUDGET_USD%"=="" set "OPENAI_MONTHLY_BUDGET_USD=100"
gh variable set OPENAI_MONTHLY_BUDGET_USD --body "%OPENAI_MONTHLY_BUDGET_USD%" && echo   [ok] variable OPENAI_MONTHLY_BUDGET_USD set

echo(
REM ---------------------------------------------------------------------------
REM  2) SECRETS  ->  GitHub SECRETS  (HIDDEN input via set-secret.cmd)
REM     You will be prompted; input is not echoed. Values never reach the agent.
REM ---------------------------------------------------------------------------
echo --- Required secrets (hidden input) ---
echo   OPENAI_API_KEY   : OpenAI API key for the Realtime session broker
call "%~dp0set-secret.cmd" OPENAI_API_KEY
echo(
echo   LWA_CLIENT_SECRET: Login with Amazon client secret (Authorization Code exchange)
call "%~dp0set-secret.cmd" LWA_CLIENT_SECRET
echo(

echo --- Optional secret ---
echo   PICOVOICE_ACCESS_KEY is ONLY needed if you use Porcupine for custom wake words.
echo   The default programmable wake-word engine is openWakeWord (open-source, no key).
choice /c YN /m "Set PICOVOICE_ACCESS_KEY now"
if errorlevel 2 goto after_pico
call "%~dp0set-secret.cmd" PICOVOICE_ACCESS_KEY
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
