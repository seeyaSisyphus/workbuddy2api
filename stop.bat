@echo off
echo [wb2api] Stopping wb2api.exe...
taskkill /F /IM wb2api.exe >nul 2>&1
if %errorlevel%==0 (
    echo [wb2api] Stopped successfully.
) else (
    echo [wb2api] wb2api.exe is not running.
)
