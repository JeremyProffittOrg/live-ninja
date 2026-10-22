@echo off
setlocal
cd /d "%~dp0.."
where python >nul 2>&1 || (echo Python is required. & exit /b 1)
python -B "%~dp0alexa-skill.py" %*
exit /b %errorlevel%
