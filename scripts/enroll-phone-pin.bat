@echo off
setlocal
if not "%~2"=="" goto invalid
if "%~1"=="" goto enroll
if not "%~1"=="--replace-existing" goto invalid
python -B "%~dp0enroll-phone-pin.py" --replace-existing
exit /b %errorlevel%
:enroll
python -B "%~dp0enroll-phone-pin.py"
exit /b %errorlevel%
:invalid
echo Usage: enroll-phone-pin.bat [--replace-existing]
exit /b 1
