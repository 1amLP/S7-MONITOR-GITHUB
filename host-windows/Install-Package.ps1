# Requires an actually signed publisher release. Included development binaries are unsigned.
[CmdletBinding(SupportsShouldProcess=$true,ConfirmImpact='High')]
param(
 [Parameter(Mandatory)][string]$Package,
 [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{40}$')][string]$PublisherThumbprint,
 [ValidateSet('all','monitor','camera')][string]$Component='all',
 [string]$Checker,
 [string]$DevCon,
 [string]$SignTool,
 [string]$SetupFile,
 [string]$DeliveryDirectory,
 [switch]$CheckOnly,
 [string]$LogDirectory,
 [switch]$UpgradeCamera,
 [switch]$UpgradeMonitor,
 [switch]$RestartCameraService,
 [AllowEmptyString()][ValidatePattern('^([0-9A-Fa-f]{40})?$')][string]$LocalTestPublisher,
 [ValidateSet('modern')][string]$CameraBackend='modern',
 [switch]$RemoveCamera
)
$ErrorActionPreference='Stop'
. (Join-Path $PSScriptRoot 'PackageCommon.ps1')
$Package=(Resolve-Path -LiteralPath $Package).Path
if($RemoveCamera -and $Component -ne 'camera'){throw 'RemoveCamera requires Component camera; monitor is never removed implicitly'}
. (Join-Path $PSScriptRoot 'camera/Backend.ps1')
$arch=Get-S7NativeArchitecture
Assert-S7NativePowerShell -Architecture $arch
if(-not $Checker){$Checker=Join-Path $PSScriptRoot "packagecheck/build/$arch/S7PackageCheck.exe"}
$Checker=(Resolve-Path -LiteralPath $Checker).Path
Assert-S7PEArchitecture -Path $Checker -Architecture $arch
Assert-S7Publisher -Path $Checker -Thumbprint $PublisherThumbprint
# Checker output never executes code from the candidate. Invalid/unsigned files stop here.
Write-Output 'S7: verifying package files and signatures'
$raw=& $Checker --package $Package --component $Component
$code=$LASTEXITCODE
if($code -ne 0){throw "Package preflight failed ($code): $($raw -join [Environment]::NewLine)"}
$preflight=($raw -join [Environment]::NewLine)|ConvertFrom-Json
if($preflight.schema -ne 'S7_WINDOWS_PREFLIGHT_1' -or -not $preflight.preflight_pass -or -not $preflight.trust_checked -or $preflight.environment.static_only){throw 'A content-only or malformed report cannot authorize installation'}
if($SetupFile){
 $SetupFile=(Resolve-Path -LiteralPath $SetupFile).Path
 Assert-S7PEArchitecture $SetupFile $arch
 Assert-S7Publisher $SetupFile $PublisherThumbprint
}
if($Component -ne 'camera' -and -not $DevCon -and -not $SetupFile){throw 'Signed S7Setup from the delivery required for monitor installation'}
$backend=$preflight.camera_backend
if($backend -cne 'modern'){throw 'Only the Windows 11 modern camera backend is included'}
if($Component -ne 'monitor'){
 Write-Output 'S7: verifying the camera backend'
 if([Environment]::OSVersion.Version.Build -lt 22000){throw 'Windows 11 is required for this camera backend'}
 if($backend -ne $CameraBackend){throw 'Wrong camera package backend'}
 Assert-S7CameraBackend $backend
 foreach($name in @('S7Camera.dll','S7CameraManage.exe')){
  Assert-S7Publisher -Path (Join-Path (Join-Path $Package 'camera') $name) -Thumbprint $PublisherThumbprint
 }
 if($RemoveCamera){
  & "$PSScriptRoot/camera/Remove-Local.ps1" -BuildDirectory (Join-Path $Package 'camera') -PublisherThumbprint $PublisherThumbprint -CheckOnly
 }
}
if($Component -ne 'camera'){
 Write-Output 'S7: verifying monitor driver ownership'
 # Read-only CAT/member/tool checks also run before CheckOnly returns.
 & "$PSScriptRoot/monitor/Install-Signed.ps1" -Package (Join-Path $Package 'monitor') -DevCon $DevCon -SignTool $SignTool -SetupFile $SetupFile -PublisherThumbprint $PublisherThumbprint -Upgrade:$UpgradeMonitor -LocalTestPublisher $LocalTestPublisher -CheckOnly
}
if($CheckOnly){
 [ordered]@{Schema='S7_WINDOWS_INSTALL_CHECK_1';CheckOnly=$true;Component=$Component;CameraBackend=$backend;RemoveCamera=[bool]$RemoveCamera;PreflightPassed=$true;Installed=$false;HardwareTested=$false}|ConvertTo-Json
 return
}
if(-not $PSCmdlet.ShouldProcess("S7 package: $Component",'Deploy verified components; explicit camera removal only; no signing-policy changes')){return}
if(-not $LogDirectory){$LogDirectory=Join-Path ([IO.Path]::GetTempPath()) ('s7-install-'+[Guid]::NewGuid().ToString('N'))}
$LogDirectory=[IO.Path]::GetFullPath($LogDirectory)
if($LogDirectory.StartsWith($Package+[IO.Path]::DirectorySeparatorChar,[StringComparison]::OrdinalIgnoreCase) -or $LogDirectory -eq $Package){throw 'Logs must be outside the immutable candidate'}
if(Test-Path -LiteralPath $LogDirectory){throw 'Use a new LogDirectory'}
New-Item -ItemType Directory -Path $LogDirectory|Out-Null
$receipt=[ordered]@{Schema='S7_WINDOWS_INSTALL_ATTEMPT_1';Success=$false;CameraBackend=$backend;RemoveCamera=[bool]$RemoveCamera;Monitor='not_requested';Camera='not_requested';RebootRequired=$false;Error='';HardwareTested=$false}
try {
 if($Component -ne 'camera'){
  $receipt.Monitor='started'
  & "$PSScriptRoot/monitor/Install-Signed.ps1" -Package (Join-Path $Package 'monitor') -DevCon $DevCon -SignTool $SignTool -SetupFile $SetupFile -PublisherThumbprint $PublisherThumbprint -Upgrade:$UpgradeMonitor -LocalTestPublisher $LocalTestPublisher -Confirm:$false
  $receipt.RebootRequired=($LASTEXITCODE -eq 1)
  $receipt.Monitor=if($receipt.RebootRequired){'staged_reboot_required'}else{'installed_not_hardware_tested'}
  if($receipt.RebootRequired){throw 'Monitor requests a reboot. Camera was not changed; installation is incomplete.'}
 }
 if($Component -ne 'monitor'){
  $receipt.Camera='started'
  if($RemoveCamera){
   & "$PSScriptRoot/camera/Remove-Local.ps1" -BuildDirectory (Join-Path $Package 'camera') -PublisherThumbprint $PublisherThumbprint -LogDirectory (Join-Path $LogDirectory 'camera') -Confirm:$false
  } else {
   & "$PSScriptRoot/camera/Install-Local.ps1" -BuildDirectory (Join-Path $Package 'camera') -PublisherThumbprint $PublisherThumbprint -LogDirectory (Join-Path $LogDirectory 'camera') -Upgrade:$UpgradeCamera -RestartCameraService:$RestartCameraService -DeliveryDirectory $DeliveryDirectory -Confirm:$false
  }
  $camera=Get-Content -LiteralPath (Join-Path $LogDirectory 'camera/install-result.json') -Raw|ConvertFrom-Json
  if(-not $camera.Success){throw 'Camera installer did not report success'}
  $receipt.Camera=if($RemoveCamera){'removed'}else{'registered_not_hardware_tested'}
 }
 $receipt.Success=$true
} catch {
 $receipt.Error=$_.Exception.Message
 $cameraReceipt=Join-Path $LogDirectory 'camera/install-result.json'
 if(Test-Path -LiteralPath $cameraReceipt){
  $failed=Get-Content -LiteralPath $cameraReceipt -Raw|ConvertFrom-Json
  if($failed.RebootRequired){$receipt.RebootRequired=$true}
  $receipt.Camera='incomplete_see_camera_receipt'
 }
 # Do not blindly remove a working monitor when a different component fails.
} finally {
 $receipt|ConvertTo-Json -Depth 4|Set-Content -LiteralPath (Join-Path $LogDirectory 'install-result.json') -Encoding UTF8
 Write-Output "Receipt: $(Join-Path $LogDirectory 'install-result.json')"
}
if(-not $receipt.Success){throw $receipt.Error}
