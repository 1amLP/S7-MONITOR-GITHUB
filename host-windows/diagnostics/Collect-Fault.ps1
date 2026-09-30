#requires -Version 7.2
[CmdletBinding()]
param(
 [Parameter(Mandatory)][string]$OutputDirectory,
 [ValidateRange(1,1440)][int]$Minutes=30,
 [ValidateRange(100,20000)][int]$MaxEvents=10000
)
$ErrorActionPreference='Stop'
$out=[IO.Path]::GetFullPath($OutputDirectory)
$repo=[IO.Path]::GetFullPath((Split-Path (Split-Path $PSScriptRoot -Parent) -Parent))
if($out -eq $repo -or $out.StartsWith($repo+[IO.Path]::DirectorySeparatorChar,[StringComparison]::OrdinalIgnoreCase)){throw 'Fault evidence must stay outside the source repository'}
if(Test-Path -LiteralPath $out){throw 'OutputDirectory must be new'}
$start=Get-Date
$since=$start.AddMinutes(-$Minutes)
$failures=[Collections.Generic.List[string]]::new()
$events=@()
foreach($log in @('Application','System')){
 try{
  $records=Get-WinEvent -FilterHashtable @{LogName=$log;StartTime=$since} -MaxEvents $MaxEvents -ErrorAction Stop
  $events+=@($records | Where-Object {
   $_.ProviderName -in @('S7Monitor','S7Camera','S7EndpointSync','Microsoft-Windows-Kernel-PnP','Microsoft-Windows-DriverFrameworks-UserMode','Microsoft-Windows-USB-USBHUB3','Microsoft-Windows-USB-USBXHCI')
  } | ForEach-Object {
   [ordered]@{At=$_.TimeCreated;Log=$log;Provider=$_.ProviderName;Id=$_.Id;Level=$_.Level;RecordId=$_.RecordId;Data=@($_.Properties | ForEach-Object Value)}
  })
 }catch{[void]$failures.Add("${log}: $($_.Exception.Message)")}
}
$devices=@()
try{
 $devices=@(Get-PnpDevice | Where-Object { $_.FriendlyName -like '*S7*' -or $_.InstanceId -like 'USB\VID_04E8&PID_A7C*' } | Select-Object Status,Class,FriendlyName,InstanceId,Problem)
}catch{[void]$failures.Add("PnP: $($_.Exception.Message)")}
$report=[ordered]@{
 Schema='PERIMODE_HOST_FAULT_1';CapturedAt=$start.ToUniversalTime();Since=$since.ToUniversalTime()
 ReadOnly=$true;VideoSaved=$false;AudioSaved=$false;Events=$events;Devices=$devices;QueryErrors=@($failures)
}
New-Item -ItemType Directory -Path $out | Out-Null
$path=Join-Path $out 'windows-fault.json'
$report | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $path -Encoding utf8NoBOM
$checked=Get-Content -Raw -LiteralPath $path | ConvertFrom-Json
if($checked.Schema -ne $report.Schema -or @($checked.Events).Count -ne $events.Count){throw 'Fault report readback mismatch'}
[pscustomobject]@{Path=$path;Events=$events.Count;Devices=$devices.Count;QueryErrors=@($failures)}
