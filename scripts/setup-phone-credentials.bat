@echo off
setlocal
REM User entry point. Values are read only by the PowerShell helper, never by cmd.
set "SETUP_OPTION="
if "%~1"=="" goto run
if /i "%~1"=="--check" set "SETUP_OPTION=-CheckOnly"
if /i "%~1"=="--replace-existing" set "SETUP_OPTION=-ReplaceExisting"
if not "%~2"=="" goto usage
if not defined SETUP_OPTION goto usage
:run
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0setup-phone-credentials.ps1" %SETUP_OPTION%
set "SETUP_RESULT=%ERRORLEVEL%"
echo.
pause
exit /b %SETUP_RESULT%
:usage
echo Usage: %~nx0 [--check ^| --replace-existing]
exit /b 2
