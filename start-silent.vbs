Option Explicit
Dim objShell, objWMIService, colProcesses, strCurDir, fso

Set objShell = CreateObject("WScript.Shell")
Set fso = CreateObject("Scripting.FileSystemObject")

' Get current script directory
strCurDir = fso.GetParentFolderName(WScript.ScriptFullName)
objShell.CurrentDirectory = strCurDir

' Check if wb2api.exe is already running
Set objWMIService = GetObject("winmgmts:\\.\root\cimv2")
Set colProcesses = objWMIService.ExecQuery("Select * from Win32_Process Where Name = 'wb2api.exe'")

If colProcesses.Count = 0 Then
    ' Proxy env vars are cleared inside the gateway itself (cmd/server/main.go clearProxyEnv),
    ' so direct-connect works no matter how the process is started. The three
    ' objShell.Environment("PROCESS").Remove lines that used to live here were removed on
    ' 2026-10-01: script-level env changes are NOT inherited by the child spawned via
    ' objShell.Run (verified 2026-09-30 20:41: process started by this script still dialed
    ' 127.0.0.1:7890 while the 20:46 start from the same script did not).
    ' Launch wb2api.exe silently (0 = hidden window, False = do not wait)
    objShell.Run "cmd.exe /c ""set WB2A_PYTHON=python && """"" & strCurDir & "\wb2api.exe"" -config config.json >> """ & strCurDir & "\server.out.log"" 2>> """ & strCurDir & "\server.err.log""""", 0, False
End If