[CmdletBinding()]
param()
$ErrorActionPreference='Stop'
. (Join-Path $PSScriptRoot 'DeviceIdentity.ps1')
Get-S7MonitorDevice | Format-Table Status,Class,FriendlyName,InstanceId -AutoSize
Get-PnpDevice -PresentOnly | Where-Object {$_.InstanceId -match '^USB\\VID_04E8&PID_(A7C0|A7C1|6860)(&|\\)'} | Format-Table Status,Class,FriendlyName,InstanceId -AutoSize
Get-WinEvent -FilterHashtable @{LogName='System';ProviderName='Microsoft-Windows-DriverFrameworks-UserMode';StartTime=(Get-Date).AddMinutes(-30)} -ErrorAction SilentlyContinue | Select-Object TimeCreated,Id,LevelDisplayName,Message
# Read-only. Capture OutputDebugString with the approved WDK debugger for S7Monitor HRESULTs.
