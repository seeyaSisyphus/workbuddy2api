@echo off
cd /d %~dp0
echo [wb2api] Starting WorkBuddy2API in foreground (Ctrl+C to stop)...
wb2api.exe -config config.json
pause
