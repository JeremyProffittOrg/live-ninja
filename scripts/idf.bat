@echo off
REM ESP-IDF wrapper for the Tab5 firmware. Git Bash sets MSYSTEM, which
REM makes export.bat abort; this file must be invoked from cmd.exe.
setlocal
set "MSYSTEM="
set "IDF_PYTHON_ENV_PATH=%USERPROFILE%\.espressif\python_env\idf5.4_py3.13_env"
call C:\esp\esp-idf-v5.4.4\export.bat
if errorlevel 1 exit /b 1
cd /d C:\dev\live-ninja\firmware
idf.py %*
