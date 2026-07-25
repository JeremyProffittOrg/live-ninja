@echo off
REM ============================================================================
REM  set-secret.bat - native Windows version of set-secret.sh (no Git Bash).
REM  A sanctioned way to put a secret on this repo from cmd/PowerShell.
REM  (set-secret.cmd is a legacy shim that still requires Git Bash; this one
REM  does not. PATHEXT resolves `set-secret` to this .bat before the .cmd.)
REM
REM  Agents: run this and let the USER type/paste the value at gh's hidden
REM  prompt. The value never appears in chat, argv, logs, or batch variables
REM  (interactive mode pipes nothing through this script - gh itself reads the
REM  value; that is also why no sha256 fingerprint is printed, unlike the .sh:
REM  there is no value in scope to hash). Verification = updated_at via gh api.
REM
REM    scripts\set-secret.bat SECRET_NAME                (gh's hidden prompt)
REM    scripts\set-secret.bat SECRET_NAME --file PATH    (multiline: keys/certs;
REM                                                       delete PATH afterward)
REM    scripts\set-secret.bat SECRET_NAME --generate     (random 48-byte base64)
REM
REM  Requires: gh (authenticated), run from inside the repo clone.
REM ============================================================================
setlocal
set "NAME=%~1"
set "MODE=%~2"
set "FILE=%~3"

if "%NAME%"=="" (
  echo usage: %~nx0 SECRET_NAME [--file PATH ^| --generate] 1>&2
  exit /b 2
)
REM findstr's character ranges are NOT reliably case-sensitive (collation
REM dependent), so `findstr /r /x "[A-Z][A-Z0-9_]*"` happily accepted a
REM lowercase name and this script went on to set a real secret under it.
REM PowerShell's -cmatch is explicitly case-sensitive; use that instead.
powershell -NoProfile -Command "if ($env:NAME -cmatch '^[A-Z][A-Z0-9_]*$') { exit 0 } else { exit 1 }" || (
  echo ERROR: secret names are UPPER_SNAKE_CASE 1>&2
  exit /b 2
)

where gh >nul 2>&1 || (echo ERROR: gh CLI not found 1>&2 & exit /b 1)
gh auth status >nul 2>&1 || (echo ERROR: gh not authenticated ^(run: gh auth login^) 1>&2 & exit /b 1)

cd /d "%~dp0.."
set "REPO="
for /f "delims=" %%r in ('gh repo view --json nameWithOwner --jq .nameWithOwner 2^>nul') do set "REPO=%%r"
if "%REPO%"=="" (echo ERROR: cannot resolve repo from origin remote 1>&2 & exit /b 1)

if "%MODE%"=="--file" goto mode_file
if "%MODE%"=="--generate" goto mode_generate
if "%MODE%"=="" goto mode_prompt
echo ERROR: unknown option '%MODE%' 1>&2
exit /b 2

:mode_prompt
REM Refuse to run the interactive path without a real console. gh reads the
REM value from stdin, so when this is piped or run by an agent it sees EOF and
REM silently sets an EMPTY secret - which is exactly what happened once.
powershell -NoProfile -Command "if ([Console]::IsInputRedirected) { exit 1 } else { exit 0 }" || (
  echo ERROR: interactive prompt needs a terminal. Agents: run this in the user's 1>&2
  echo        terminal, or use --file / --generate. Do NOT pipe a value in. 1>&2
  exit /b 1
)
echo Enter/paste the value for %NAME% at gh's hidden prompt (input is not echoed):
gh secret set "%NAME%" -R "%REPO%" || goto fail
goto verify

:mode_file
if "%FILE%"=="" (echo ERROR: --file needs a PATH 1>&2 & exit /b 1)
if not exist "%FILE%" (echo ERROR: file '%FILE%' missing 1>&2 & exit /b 1)
for %%z in ("%FILE%") do if "%%~zz"=="0" (echo ERROR: file '%FILE%' is empty 1>&2 & exit /b 1)
gh secret set "%NAME%" -R "%REPO%" < "%FILE%" || goto fail
echo Reminder: delete "%FILE%" now that the secret is set.
goto verify

:mode_generate
powershell -NoProfile -Command "$b=[byte[]]::new(48);[System.Security.Cryptography.RandomNumberGenerator]::Fill($b);[Console]::Out.Write([Convert]::ToBase64String($b))" | gh secret set "%NAME%" -R "%REPO%" || goto fail
goto verify

:verify
set "UPDATED="
for /f "delims=" %%u in ('gh api "repos/%REPO%/actions/secrets/%NAME%" --jq .updated_at 2^>nul') do set "UPDATED=%%u"
if "%UPDATED%"=="" (echo ERROR: secret %NAME% not visible via API after set 1>&2 & exit /b 1)
echo OK: %NAME% set on %REPO% (updated_at %UPDATED%)
echo Reminder: secrets reach running stacks only on the next deploy (push to main).
endlocal
exit /b 0

:fail
echo ERROR: gh secret set failed for %NAME% 1>&2
exit /b 1
