Option Explicit

Dim shell
Dim fso
Dim baseDir
Dim processEnv
Dim password
Dim command

Set shell = CreateObject("WScript.Shell")
Set fso = CreateObject("Scripting.FileSystemObject")
baseDir = fso.GetParentFolderName(WScript.ScriptFullName)
Set processEnv = shell.Environment("PROCESS")

If Len(Trim(processEnv("FENGCHUAN_PASSWORD"))) = 0 Then
    password = InputBox("Enter the FengChuan access password:", "FengChuan", "")
    If Len(password) = 0 Then
        WScript.Quit 0
    End If
    processEnv("FENGCHUAN_PASSWORD") = password
End If

command = "powershell.exe -NoProfile -STA -WindowStyle Hidden -ExecutionPolicy Bypass -File """ _
    & baseDir & "\fengchuan-tray.ps1"""

shell.Run command, 0, False
