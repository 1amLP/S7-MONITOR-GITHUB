[CmdletBinding(SupportsShouldProcess=$true,ConfirmImpact='High')]
param([Parameter(Mandatory)][string]$BuildDirectory,
 [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{40}$')][string]$PublisherThumbprint,
 [switch]$CheckOnly,[string]$LogDirectory)
$ErrorActionPreference='Stop'
. (Join-Path (Split-Path $PSScriptRoot -Parent) 'PackageCommon.ps1')
. (Join-Path $PSScriptRoot 'Backend.ps1')
$arch=Get-S7NativeArchitecture;Assert-S7NativePowerShell $arch
if([Environment]::OSVersion.Version.Build -lt 22000){throw 'Use this modern removal on Windows build22000+'}
$BuildDirectory=(Resolve-Path -LiteralPath $BuildDirectory).Path
function Get-OwnedModern {
 Assert-S7CameraBackend 'modern'
 $classes=@(Get-S7CameraRegistration $S7ModernClasses)
 if($classes.Count -eq 0){return [pscustomobject]@{Classes=@();Manager=$null}}
 if($classes.Count -ne 2){throw 'Incomplete modern COM pair; automatic removal refused'}
 $dirs=@()
 foreach($c in $classes){
  if($c.Key -notlike 'HKLM:\*' -or (Split-Path $c.Dll -Leaf) -ne 'S7Camera.dll'){throw 'Unowned modern camera registration'}
  $allowed=[IO.Path]::GetFullPath((Join-Path $env:ProgramFiles 'S7 Appliance\Camera'))+'\'
  if(-not [IO.Path]::GetFullPath($c.Dll).StartsWith($allowed,[StringComparison]::OrdinalIgnoreCase)){throw 'Unowned camera path'}
  $dirs+=Split-Path $c.Dll -Parent
 }
 $dirs=@($dirs|Sort-Object -Unique)
 if($dirs.Count -ne 1){throw 'Modern camera paths differ'}
 foreach($n in @('S7Camera.dll','S7CameraManage.exe')){
  $installed=Join-Path $dirs[0] $n;$candidate=Join-Path $BuildDirectory $n
  foreach($p in @($installed,$candidate)){
   Assert-S7RegularPath $p;Assert-S7PEArchitecture $p $arch;Assert-S7Publisher $p $PublisherThumbprint
  }
  if((Get-FileHash -LiteralPath $installed).Hash -ne (Get-FileHash -LiteralPath $candidate).Hash){throw 'Removal requires the exact installed modern package'}
 }
 return [pscustomobject]@{Classes=$classes;Manager=(Join-Path $dirs[0] 'S7CameraManage.exe')}
}
$owned=Get-OwnedModern
if($CheckOnly){Write-Output 'Modern removal ownership checks passed; no mutation.';return}
$who=[Security.Principal.WindowsIdentity]::GetCurrent()
if(-not ([Security.Principal.WindowsPrincipal]$who).IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)){throw 'Administrator required'}
if(-not $PSCmdlet.ShouldProcess('Only S7 modern virtual camera pair','Remove endpoints and owned COM registration')){return}
if(-not $LogDirectory){$LogDirectory=Join-Path ([IO.Path]::GetTempPath()) ('s7-remove-'+[Guid]::NewGuid().ToString('N'))}
$LogDirectory=[IO.Path]::GetFullPath($LogDirectory)
if($LogDirectory -eq $BuildDirectory -or $LogDirectory.StartsWith($BuildDirectory+'\',[StringComparison]::OrdinalIgnoreCase) -or (Test-Path -LiteralPath $LogDirectory)){throw 'Use new external log directory'}
New-Item -ItemType Directory -Path $LogDirectory|Out-Null
$receipt=[ordered]@{Success=$false;Removed=$false;Error='';FilesRetained=$true;HardwareTested=$false}
$serviceStopped=$false
$mutex=New-Object Threading.Mutex($false,'Global\S7NativeCameraDeployment-v2');$locked=$false
try {
 try {$locked=$mutex.WaitOne(0)} catch [Threading.AbandonedMutexException] {$locked=$true}
 if(-not $locked){throw 'Another S7 camera deployment is running'}
 $owned=Get-OwnedModern
 if($owned.Manager){
  $service=Get-CimInstance Win32_Service -Filter "Name='S7EndpointSync'"
  if($service){
   if($service.PathName -ne ('"'+$owned.Manager+'" service') -or $service.StartName -ne 'LocalSystem'){throw 'Endpoint service ownership mismatch'}
   $serviceStopped=$service.State -eq 'Running'
   Stop-Service S7EndpointSync -ErrorAction Stop
   (Get-Service S7EndpointSync).WaitForStatus('Stopped',[TimeSpan]::FromSeconds(20))
  }
  & $owned.Manager remove | Out-Host
  if($LASTEXITCODE -ne 0){throw 'Endpoint removal failed; COM and files retained'}
  $receipt.Removed=$true
  & "$PSScriptRoot/Inspect-Runtime.ps1" -OutputFile (Join-Path $LogDirectory 'runtime.json') -PublisherThumbprint $PublisherThumbprint -ReleaseLoadedSource | Out-Host
  $runtime=Get-Content -LiteralPath (Join-Path $LogDirectory 'runtime.json') -Raw|ConvertFrom-Json
  if(-not $runtime.Success){throw 'Source still loaded; COM retained. Close clients and retry; FrameServer is not stopped.'}
  foreach($c in $owned.Classes){
   $now=(Get-Item -LiteralPath (Join-Path $c.Key 'InprocServer32')).GetValue('')
   if([Environment]::ExpandEnvironmentVariables($now) -ne $c.Dll){throw 'COM ownership changed; no registry removal'}
  }
  foreach($c in $owned.Classes){Remove-Item -LiteralPath $c.Key -Recurse}
  if($service){
   & "$env:SystemRoot/System32/sc.exe" delete S7EndpointSync
   if($LASTEXITCODE){throw 'Endpoint service removal failed'}
   $serviceStopped=$false
  }
 }
 $receipt.Success=$true
} catch {$receipt.Error=$_.Exception.Message} finally {
 if($serviceStopped -and -not $receipt.Success){
  try{Start-Service S7EndpointSync -ErrorAction Stop}catch{$receipt.ServiceRestoreError=$_.Exception.Message}
 }
 if($locked){$mutex.ReleaseMutex()};$mutex.Dispose()
 $receipt|ConvertTo-Json|Set-Content -LiteralPath (Join-Path $LogDirectory 'install-result.json') -Encoding UTF8
}
if(-not $receipt.Success){throw $receipt.Error}
