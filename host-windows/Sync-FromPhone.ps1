# Installed signed entry. No developer paths, downloaded scripts or partial ZIP
# execute before publisher, identity, architecture and payload verification.
[CmdletBinding()]
param([Parameter(Mandatory)][uint32]$Release)
$ErrorActionPreference='Stop'
. (Join-Path $PSScriptRoot 'PackageCommon.ps1')
. (Join-Path $PSScriptRoot 'PackageState.ps1')
. (Join-Path $PSScriptRoot 'PhonePackage.ps1')
$mutex=[Threading.Mutex]::new($false,'Global\S7PhonePackageDeployment')
$locked=$false
$exitCode=1
$report=[ordered]@{Schema='S7_PHONE_PACKAGE_SYNC_1';Release=$Release;Source='S7 USB';Success=$false;Stage='read phone';Changed=$false;Error='';HardwareTested=$false}
try {
 try {$locked=$mutex.WaitOne(0)} catch [Threading.AbandonedMutexException] {$locked=$true}
 if(-not $locked){$exitCode=1618;throw 'S7 package update already running'}
 $who=[Security.Principal.WindowsIdentity]::GetCurrent()
 if(-not ([Security.Principal.WindowsPrincipal]$who).IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)){throw 'Elevated package service required'}
 $own=Get-AuthenticodeSignature -LiteralPath $PSCommandPath
 if($own.Status -ne 'Valid'){throw 'Installed updater signature invalid'}
 $publisher=$own.SignerCertificate.Thumbprint
 $arch=Get-S7NativeArchitecture
 Assert-S7NativePowerShell $arch
 $ownDelivery=Get-S7SignedBundleManifest ([IO.File]::ReadAllBytes((Join-Path $PSScriptRoot 'bundle.json'))) ([IO.File]::ReadAllBytes((Join-Path $PSScriptRoot 'bundle.p7s'))) $publisher
 $transport=Join-Path $PSScriptRoot 'monitor/ReadOnlyDiagnostics.cs'
 $transportPin=@($ownDelivery.files|Where-Object {$_.path -ceq 'host/monitor/ReadOnlyDiagnostics.cs'})
 if($transportPin.Count -ne 1 -or (Get-FileHash -LiteralPath $transport).Hash -ne $transportPin[0].sha256){throw 'Installed USB client differs from signed helper'}
 Add-Type -Path $transport
 if($ownDelivery.device_serial -cnotmatch '^[0-9a-f]{18}$'){throw 'Signed device serial is missing'}
 [S7ReadOnlyDiagnostics]::ExpectedSerial=$ownDelivery.device_serial
 $packet=[S7ReadOnlyDiagnostics]::PackageInfo()
 if([BitConverter]::ToUInt32($packet,8) -ne 2){throw 'S7 package is unavailable'}
 $info=[Text.Encoding]::UTF8.GetString($packet,32,[BitConverter]::ToUInt32($packet,12))|ConvertFrom-Json
 if($info.schema -cne 'S7_PHONE_PACKAGE_1' -or $info.release -ne $Release -or $info.bytes -le 0 -or $info.bytes -gt 128MB -or $info.sha256 -cnotmatch '^[0-9a-f]{64}$'){throw 'Invalid phone package identity'}
 $base=Join-Path $env:ProgramFiles 'S7 Appliance/Delivery'
 if(Test-Path -LiteralPath $base){
  if((Get-Item -LiteralPath $base).Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'Delivery root is a reparse point'}
 }else{New-Item -ItemType Directory -Path $base|Out-Null}
 $attempt=Join-Path $base ([Guid]::NewGuid().ToString('N'))
 New-Item -ItemType Directory -Path $attempt|Out-Null
 $reportPath=Join-Path $attempt 'result.json'
 $archive=Join-Path $base ($info.sha256+'.zip')
 $report.Stage='receive signed package from S7'
 if(Test-Path -LiteralPath $archive){
  $cached=Get-Item -LiteralPath $archive
  if($cached.PSIsContainer -or ($cached.Attributes -band [IO.FileAttributes]::ReparsePoint) -or $cached.Length -ne $info.bytes -or (Get-FileHash -LiteralPath $archive).Hash -ne $info.sha256){throw 'Cached package was altered; not overwritten'}
 }else{
  $partial=Join-Path $attempt 'package.part'
  [S7ReadOnlyDiagnostics]::DownloadPackage($Release,[long]$info.bytes,$info.sha256,$partial)
  Move-Item -LiteralPath $partial -Destination $archive
 }
 $root=Join-Path $base ($info.sha256+'.verified')
 if(Test-Path -LiteralPath $root){
  if((Get-Item -LiteralPath $root).Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'Verified cache is a reparse point'}
  $delivery=Assert-S7ExpandedPackage $root $Release $publisher
 }else{
  $stage=Join-Path $attempt 'verified'
  $delivery=Expand-S7PhonePackage -Archive $archive -Destination $stage -Release $Release -PublisherThumbprint $publisher
  Move-Item -LiteralPath $stage -Destination $root
 }
 if($delivery.architecture -ne $arch -and $delivery.architecture -ne 'multi'){throw 'Phone package does not match native Windows architecture'}
 $binaryRoot=if($delivery.architecture -eq 'multi'){Join-Path $root $arch}else{$root}
 $package=Join-Path $binaryRoot 'package'
 $report.Stage='compare installed package'
 $before=Get-S7InstalledPackageState -Package $package -PublisherThumbprint $publisher -DeliveryDirectory "$root/host"
 $report.Before=$before
 if(-not $before.Matches){
  $report.Stage='replace mismatching package'
  $changes=@($before.Components|Where-Object {-not $_.Matches})
  $monitorChanged=@($changes|Where-Object {$_.Component -in @('Monitor','MonitorVersion')}).Count -gt 0
  $cameraChanged=@($changes|Where-Object {$_.Component -notin @('Monitor','MonitorVersion')}).Count -gt 0
  $component=if($monitorChanged -and $cameraChanged){'all'}elseif($monitorChanged){'monitor'}else{'camera'}
  foreach($script in @('Install-Package.ps1','PackageCommon.ps1','PackageState.ps1','PhonePackage.ps1','Sync-FromPhone.ps1','monitor/Install-Signed.ps1','monitor/DeviceIdentity.ps1','camera/Install-Local.ps1','camera/Inspect-Runtime.ps1','camera/Backend.ps1')){
   Assert-S7Publisher -Path (Join-Path "$root/host" $script) -Thumbprint $publisher
  }
  & "$root/host/Install-Package.ps1" -Package $package -PublisherThumbprint $publisher -Checker "$binaryRoot/tools/S7PackageCheck.exe" -SetupFile "$binaryRoot/tools/S7Setup.exe" -Component $component -UpgradeMonitor -UpgradeCamera -RestartCameraService -DeliveryDirectory "$root/host" -LogDirectory "$attempt/install" -Confirm:$false
  $report.Changed=$true
 }
 $report.Stage='verify installed package'
 $after=Get-S7InstalledPackageState -Package $package -PublisherThumbprint $publisher -DeliveryDirectory "$root/host"
 $report.After=$after
 if(-not $after.Matches){throw 'Installed components still differ from S7 package'}
 $report.Success=$true;$report.Stage='package matches S7'
}catch{$report.Error=$_.Exception.Message}
finally{
 if($reportPath){$report|ConvertTo-Json -Depth 8|Set-Content -LiteralPath $reportPath -Encoding UTF8}
 if($locked){$mutex.ReleaseMutex()};$mutex.Dispose()
 $report|ConvertTo-Json -Depth 8
}
if(-not $report.Success){exit $exitCode}
