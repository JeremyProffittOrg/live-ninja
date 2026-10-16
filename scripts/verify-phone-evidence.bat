@echo off
setlocal
REM Read-only evidence gate; this entrypoint cannot enable calls.
powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "%~dp0verify-phone-evidence.ps1" %*
exit /b %ERRORLEVEL%
