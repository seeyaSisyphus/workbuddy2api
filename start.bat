@echo off
rem [guardian] manual start: clear pause flag and re-arm the guardian
del /q "D:\ai-servers\ai-servers-guardian\pause-wb2api.flag" 2>nul
cd /d %~dp0
rem [scheduler] Windows has no python3; script tasks need an explicit interpreter
rem (same reason as launch-wb2api.bat, see internal/scheduler/school.go pythonCmd).
set WB2A_PYTHON=python
echo [wb2api] Starting WorkBuddy2API in foreground (Ctrl+C to stop)...
wb2api.exe -config config.json
pause
