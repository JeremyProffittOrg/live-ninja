@echo off
REM Thin wrapper so set-secret works from cmd/PowerShell too (runs the bash script via Git Bash).
bash "%~dp0set-secret.sh" %*
