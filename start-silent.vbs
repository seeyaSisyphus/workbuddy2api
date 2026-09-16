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
    ' Launch wb2api.exe silently (0 = hidden window, False = do not wait)
    objShell.Run "cmd.exe /c """"" & strCurDir & "\wb2api.exe"" -config config.json >> """ & strCurDir & "\server.out.log"" 2>> """ & strCurDir & "\server.err.log""""", 0, False
End If
