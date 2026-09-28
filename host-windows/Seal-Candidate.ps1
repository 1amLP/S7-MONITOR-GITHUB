# Records exact bytes AFTER publishing/signing. Hashes do not establish trust.
[CmdletBinding()]
param([Parameter(Mandatory)][string]$Directory,[Parameter(Mandatory)][ValidateSet('x64','arm64')][string]$Architecture,[Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{18}$')][string]$DeviceSerial)
$ErrorActionPreference='Stop'
$Architecture=$Architecture.ToLowerInvariant()
. (Join-Path $PSScriptRoot 'PackageCommon.ps1')
$Directory=(Resolve-Path -LiteralPath $Directory).Path
$names=@('monitor/S7Monitor.inf','monitor/S7Monitor.cat','monitor/S7Monitor.dll','camera/S7Camera.dll','camera/S7CameraManage.exe','diagnostics/camera-fps.exe')
$entries=@()
foreach($name in $names){
 $path=Join-Path $Directory $name
 $item=Get-Item -LiteralPath $path -ErrorAction Stop
 if($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)){throw "Regular file required: $name"}
 if($item.Length -le 0 -or $item.Length -gt 256MB){throw "Payload size rejected: $name"}
 if($name -match '\.(dll|exe)$'){Assert-S7PEArchitecture -Path $path -Architecture $Architecture}
 $entries+=@{path=$name;bytes=$item.Length;sha256=(Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()}
}
# No auto-cleanup: unknown files are an error, not files to remove.
foreach($item in Get-ChildItem -LiteralPath $Directory -Recurse -Force){
 if($item.Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'Reparse point refused'}
 if(-not $item.PSIsContainer){
  $relative=$item.FullName.Substring($Directory.Length+1).Replace('\','/')
  if($relative -cnotin ($names+@('package.json'))){throw "Unlisted candidate file: $relative"}
 }
}
[ordered]@{schema='S7_WINDOWS_CANDIDATE_2';architecture=$Architecture;camera_backend='modern';device_serial=$DeviceSerial.ToLowerInvariant();files=$entries}|ConvertTo-Json -Depth 4|Set-Content -LiteralPath (Join-Path $Directory 'package.json') -Encoding UTF8
