# Signed entry on the S7 read-only USB disk. No preinstalled S7 program required.
[CmdletBinding()]
param([Parameter(Mandatory)][string]$MediaRoot)
$ErrorActionPreference='Stop'
. (Join-Path $PSScriptRoot 'PackageCommon.ps1')
. (Join-Path $PSScriptRoot 'PhonePackage.ps1')
$publisher=Get-AuthenticodeSignature -LiteralPath $PSCommandPath
if($publisher.Status -ne 'Valid'){throw 'S7 installer signature is not trusted by Windows'}
$publisher=$publisher.SignerCertificate.Thumbprint
$release=0;$failure=$null;$attempt=$null
$deploymentMutex=$null;$deploymentLocked=$false
function Test-S7InstallerParent([string]$Id){
 for($i=0;$i -lt 12 -and $Id;$i++){
  if($Id -ieq ('USB\VID_04E8&PID_A7C3\'+$script:deviceSerial)){return $true}
  $Id=(Get-PnpDeviceProperty -InstanceId $Id -KeyName DEVPKEY_Device_Parent -ErrorAction SilentlyContinue).Data
 }
 return $false
}
function Complete-S7Installer([uint32]$Release){
 foreach($port in @(Get-CimInstance Win32_SerialPort -ErrorAction SilentlyContinue)){
  if($port.DeviceID -notmatch '^COM[1-9][0-9]{0,3}$' -or -not(Test-S7InstallerParent $port.PNPDeviceID)){continue}
  $serial=[IO.Ports.SerialPort]::new($port.DeviceID,115200)
  try{
   $serial.Encoding=[Text.Encoding]::ASCII;$serial.NewLine="`n";$serial.WriteTimeout=1000
   $serial.DtrEnable=$true;$serial.RtsEnable=$true;$serial.Open()
   $serial.WriteLine("S7-INSTALL-DONE $Release");return $true
  }catch{Write-Warning $_.Exception.Message}finally{$serial.Dispose()}
 }
 return $false
}
try {
 Write-Host 'S7: checking installer media and Windows architecture'
 $who=[Security.Principal.WindowsIdentity]::GetCurrent()
 if(-not ([Security.Principal.WindowsPrincipal]$who).IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)){throw 'Windows administrator confirmation is required'}
 $MediaRoot=(Resolve-Path -LiteralPath $MediaRoot).Path
 $infoFile=Join-Path $MediaRoot 'PHONE_PACKAGE.json'
 if((Get-Item -LiteralPath $infoFile).Length -gt 4096){throw 'Installer metadata is oversized'}
 $info=Get-Content -LiteralPath $infoFile -Raw|ConvertFrom-Json
 if($info.device_serial -cnotmatch '^[0-9a-f]{18}$'){throw 'Device serial missing from installer metadata'}
 $script:deviceSerial=$info.device_serial
 $drive=[IO.Path]::GetPathRoot($MediaRoot).TrimEnd('\')
 if($drive -notmatch '^[A-Za-z]:$'){throw 'Installer must come from the S7 USB drive'}
 $disk=Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='$drive'"
 $owned=$false
 foreach($partition in @(Get-CimAssociatedInstance -InputObject $disk -Association Win32_LogicalDiskToPartition)){
  foreach($physical in @(Get-CimAssociatedInstance -InputObject $partition -Association Win32_DiskDriveToDiskPartition)){
   if(Test-S7InstallerParent $physical.PNPDeviceID){$owned=$true}
  }
 }
 if(-not $owned){throw 'Source is not the connected S7 installer disk'}
 $arch=Get-S7NativeArchitecture;Assert-S7NativePowerShell $arch
 if($info.schema -cne 'S7_PHONE_PACKAGE_1' -or $info.release -le 0 -or $info.sha256 -cnotmatch '^[0-9a-f]{64}$'){throw 'Invalid S7 package metadata'}
 $release=[uint32]$info.release
 $source=Join-Path $MediaRoot 'package.zip'
 if((Get-Item -LiteralPath $source).Length -ne $info.bytes -or $info.bytes -le 0 -or $info.bytes -gt 128MB){throw 'Invalid package length'}
 $base=Join-Path $env:ProgramFiles 'S7 Appliance/Delivery'
 if(Test-Path -LiteralPath $base){if((Get-Item -LiteralPath $base).Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'Unsafe delivery folder'}}
 else{New-Item -ItemType Directory -Path $base|Out-Null}
 $attempt=Join-Path $base ([Guid]::NewGuid().ToString('N'));New-Item -ItemType Directory -Path $attempt|Out-Null
 Write-Host 'S7: copying the package from the phone'
 $archive=Join-Path $attempt 'package.zip';Copy-Item -LiteralPath $source -Destination $archive
 if((Get-FileHash -LiteralPath $archive).Hash -ne $info.sha256){throw 'USB copy SHA256 mismatch'}
 $root=Join-Path $attempt 'verified'
 $manifest=Expand-S7PhonePackage -Archive $archive -Destination $root -Release $release -PublisherThumbprint $publisher
 if($manifest.architecture -ne $arch -and $manifest.architecture -ne 'multi'){throw 'Native Windows architecture is not present in this package'}
 $binaryRoot=if($manifest.architecture -eq 'multi'){Join-Path $root $arch}else{$root}
 foreach($name in @('PackageState.ps1','Install-Package.ps1')){Assert-S7Publisher "$root/host/$name" $publisher}
 . "$root/host/PackageState.ps1"
 # Serialize the entire read/replace/verify transaction with the USB updater.
 # COM and the service path are separate writes; neither is an ownership
 # snapshot while another approved installer is between those writes.
 $deploymentMutex=[Threading.Mutex]::new($false,'Global\S7PhonePackageDeployment')
 Write-Host 'S7: waiting for any current S7 package update to finish'
 try {$deploymentLocked=$deploymentMutex.WaitOne([TimeSpan]::FromSeconds(120))}
 catch [Threading.AbandonedMutexException] {$deploymentLocked=$true}
 if(-not $deploymentLocked){throw 'Another S7 package update is still running. Wait for it to finish, then retry.'}
 $before=Get-S7InstalledPackageState -Package "$binaryRoot/package" -PublisherThumbprint $publisher -DeliveryDirectory "$root/host"
 if(-not $before.Matches){
  Write-Host 'S7: updating only mismatching driver components'
  $changes=@($before.Components|Where-Object {-not $_.Matches})
  $monitorChanged=@($changes|Where-Object {$_.Component -in @('Monitor','MonitorVersion')}).Count -gt 0
  $cameraChanged=@($changes|Where-Object {$_.Component -notin @('Monitor','MonitorVersion')}).Count -gt 0
  $component=if($monitorChanged -and $cameraChanged){'all'}elseif($monitorChanged){'monitor'}else{'camera'}
  & "$root/host/Install-Package.ps1" -Package "$binaryRoot/package" -Component $component -PublisherThumbprint $publisher -Checker "$binaryRoot/tools/S7PackageCheck.exe" -SetupFile "$binaryRoot/tools/S7Setup.exe" -UpgradeMonitor -UpgradeCamera -RestartCameraService -DeliveryDirectory "$root/host" -LogDirectory "$attempt/install" -Confirm:$false
 }
 $after=Get-S7InstalledPackageState -Package "$binaryRoot/package" -PublisherThumbprint $publisher -DeliveryDirectory "$root/host"
 if(-not $after.Matches){throw 'Windows package verification did not pass'}
 [ordered]@{Success=$true;Source='S7 USB disk';Release=$release;Before=$before;After=$after}|ConvertTo-Json -Depth 8|Set-Content -LiteralPath "$attempt/media-result.json" -Encoding UTF8
 Write-Host 'S7: package verified. Returning the phone to device mode.'
}catch{
 $failure=$_.Exception.Message
 if($attempt){[ordered]@{Success=$false;Error=$failure;Source='S7 USB disk';Release=$release}|ConvertTo-Json|Set-Content -LiteralPath "$attempt/media-result.json" -Encoding UTF8}
 Write-Error -Message $failure -ErrorAction Continue
}finally{
 if($deploymentLocked){$deploymentMutex.ReleaseMutex()}
 if($deploymentMutex){$deploymentMutex.Dispose()}
 if($release -ne 0 -and -not(Complete-S7Installer $release)){Write-Warning 'Use Return to devices on S7; the completion channel is unavailable.'}
}
if($failure){Add-Type -AssemblyName System.Windows.Forms;[Windows.Forms.MessageBox]::Show($failure,'S7 Setup')|Out-Null;exit 1}
