@echo off
cd /d %~dp0
echo ========================================
echo WorkBuddy2API Status
echo ========================================
tasklist /FI "IMAGENAME eq wb2api.exe" 2>NUL | find /I /N "wb2api.exe">NUL
if "%ERRORLEVEL%"=="0" (
    echo [Status] Running (wb2api.exe)
    echo [Port Listen]
    netstat -ano | findstr :7863
) else (
    echo [Status] NOT running
)
echo ========================================
pause
