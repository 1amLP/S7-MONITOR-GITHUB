#requires -Version 7.2
# Publisher/developer build orchestration. Produces an UNSIGNED candidate only.
[CmdletBinding()]
param(
 [Parameter(Mandatory)][string]$WdkRoot,
 [Parameter(Mandatory)][string]$VcRoot,
 [Parameter(Mandatory)][string]$SdkRoot,
 [Parameter(Mandatory)][string]$OutputDirectory,
 [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{18}$')][string]$DeviceSerial,
 [ValidateSet('x64','ARM64')][string]$Platform='x64',
 [string]$KitVersion='10.0.26100.0',
 [string]$WdkPackage,
 [string]$WdkToolRoot,
 [string]$UmdfVersion='2.25',
 [string]$IddCxVersion='1.9'
)
$ErrorActionPreference='Stop'
if(Test-Path -LiteralPath $OutputDirectory){throw 'OutputDirectory must be new'}
$out=[IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Path $out|Out-Null
$common=@{Platform=$Platform;VcRoot=$VcRoot;SdkRoot=$SdkRoot;KitVersion=$KitVersion}
& "$PSScriptRoot/monitor/Build-Local.ps1" @common -DeviceSerial $DeviceSerial -WdkRoot $WdkRoot -WdkPackage $WdkPackage -WdkToolRoot $WdkToolRoot -UmdfVersion $UmdfVersion -IddCxVersion $IddCxVersion -OutputDirectory "$out/monitor-build" -SkipTests
if($LASTEXITCODE){throw 'Monitor build failed'}
& "$PSScriptRoot/camera/Build-Local.ps1" @common -DeviceSerial $DeviceSerial -OutputDirectory "$out/camera-build" -SkipTests
if($LASTEXITCODE){throw 'Camera build failed'}
& "$PSScriptRoot/diagnostics/Build-FpsProbe.ps1" @common -OutputDirectory "$out/probe-build"
if($LASTEXITCODE){throw 'FPS probe build failed'}
$candidate=Join-Path $out 'candidate'
foreach($dir in @('monitor','camera','diagnostics')){New-Item -ItemType Directory -Path (Join-Path $candidate $dir)|Out-Null}
foreach($name in @('S7Monitor.dll','S7Monitor.inf','S7Monitor.cat')){Copy-Item -LiteralPath "$out/monitor-build/package/$name" -Destination "$candidate/monitor/$name"}
foreach($name in @('S7Camera.dll','S7CameraManage.exe')){Copy-Item -LiteralPath "$out/camera-build/$name" -Destination "$candidate/camera/$name"}
Copy-Item -LiteralPath "$out/probe-build/camera-fps.exe" -Destination "$candidate/diagnostics/camera-fps.exe"
& "$PSScriptRoot/Seal-Candidate.ps1" -Directory $candidate -Architecture ($Platform.ToLowerInvariant()) -DeviceSerial $DeviceSerial
[ordered]@{Schema='S7_WINDOWS_BUILD_CANDIDATE_1';Architecture=$Platform;Candidate=$candidate;Signed=$false;ProductionReady=$false;Installed=$false;HardwareTested=$false}|ConvertTo-Json|Set-Content -LiteralPath "$out/build-result.json" -Encoding UTF8
Write-Output 'Unsigned candidate built. Publisher must sign it legitimately, then reseal hashes. This is not an installable release.'
