# Read actual installed ownership and bytes. A receipt alone is not installation.
. (Join-Path $PSScriptRoot 'PackageCommon.ps1')
. (Join-Path $PSScriptRoot 'monitor/DeviceIdentity.ps1')

function Get-S7InstalledPackageState {
 [CmdletBinding()]
 param([Parameter(Mandatory)][string]$Package,
       [string]$DeliveryDirectory,
       [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{40}$')][string]$PublisherThumbprint)
 $Package=(Resolve-Path -LiteralPath $Package).Path
 $manifestPath=Join-Path $Package 'package.json'
 $file=Get-Item -LiteralPath $manifestPath
 if($file.PSIsContainer -or $file.Length -gt 64KB -or ($file.Attributes -band [IO.FileAttributes]::ReparsePoint)){throw 'Invalid package manifest'}
 $manifest=Get-Content -LiteralPath $manifestPath -Raw|ConvertFrom-Json
 $arch=Get-S7NativeArchitecture
 if($manifest.schema -ne 'S7_WINDOWS_CANDIDATE_2' -or $manifest.camera_backend -ne 'modern' -or $manifest.architecture -ne $arch){throw 'Unsupported installed-package comparison'}
 $items=[Collections.Generic.List[object]]::new()
 function Compare-S7File([string]$Component,[string]$Relative,[string]$Installed) {
  $expected=@($manifest.files|Where-Object {$_.path -ceq $Relative})
  if($expected.Count -ne 1 -or $expected[0].sha256 -cnotmatch '^[0-9a-f]{64}$'){throw "Missing unique manifest entry: $Relative"}
  $candidate=Join-Path $Package $Relative
  Assert-S7PEArchitecture $candidate $arch
  Assert-S7Publisher $candidate $PublisherThumbprint
  $candidateFile=Get-Item -LiteralPath $candidate
  if($candidateFile.Length -ne $expected[0].bytes -or (Get-FileHash -LiteralPath $candidate).Hash -ne $expected[0].sha256){throw "Candidate changed: $Relative"}
  $actualHash=$null
  if($Installed){
   $installedFile=Get-Item -LiteralPath $Installed -ErrorAction Stop
   if($installedFile.PSIsContainer -or ($installedFile.Attributes -band [IO.FileAttributes]::ReparsePoint)){throw 'Installed file is not regular'}
   Assert-S7PEArchitecture $Installed $arch
   Assert-S7Publisher $Installed $PublisherThumbprint
   $actualHash=(Get-FileHash -LiteralPath $Installed).Hash.ToLowerInvariant()
  }
  $items.Add([pscustomobject]@{Component=$Component;InstalledPath=$Installed;InstalledSHA256=$actualHash;ExpectedSHA256=$expected[0].sha256;Matches=($actualHash -ceq $expected[0].sha256)})
 }
 $nodes=@(Get-S7MonitorDevice)
 if($nodes.Count -gt 1){throw 'Ambiguous monitor ownership'}
 $monitor=$null;$driverVersion=$null
 if($nodes.Count){
  $monitor=Get-S7MonitorDriverFile $nodes[0].InstanceId
  $driverVersion=(Get-PnpDeviceProperty -InstanceId $nodes[0].InstanceId -KeyName DEVPKEY_Device_DriverVersion).Data
 }
 Compare-S7File 'Monitor' 'monitor/S7Monitor.dll' $monitor
 $inf=Get-Content -LiteralPath (Join-Path $Package 'monitor/S7Monitor.inf')
 $versions=@($inf|ForEach-Object {
  $match=[regex]::Match($_,'^\s*DriverVer\s*=\s*[^,]+,\s*(\d+\.\d+\.\d+\.\d+)\s*$')
  if($match.Success){[version]$match.Groups[1].Value}
 })
 if($versions.Count -ne 1){throw 'Missing unique driver version'}
 $items.Add([pscustomobject]@{Component='MonitorVersion';Installed=$driverVersion;Expected=$versions[0].ToString();Matches=($null -ne $driverVersion -and [version]$driverVersion -eq $versions[0])})
 $classes=@('{4C850E86-1698-44BF-893E-43ECF61BAAA9}','{14986CDD-1DA2-4A72-B953-914E65934921}')
 $managers=@()
 foreach($class in $classes){
  $key="HKLM:\SOFTWARE\Classes\CLSID\$class\InprocServer32"
  $dll=$null
  if(Test-Path -LiteralPath $key){
   $dll=(Get-Item -LiteralPath $key).GetValue('')
   $allowed=[IO.Path]::GetFullPath((Join-Path $env:ProgramFiles 'S7 Appliance/Camera'))+[IO.Path]::DirectorySeparatorChar
   if(-not [IO.Path]::GetFullPath($dll).StartsWith($allowed,[StringComparison]::OrdinalIgnoreCase)){throw 'Unowned camera registration'}
   $managers+=Join-Path (Split-Path $dll -Parent) 'S7CameraManage.exe'
  }
  Compare-S7File "Camera $class" 'camera/S7Camera.dll' $dll
 }
 $paths=@($managers|Select-Object -Unique)
 if($paths.Count -gt 1){throw 'Front and rear camera packages differ'}
 $manager=if($paths.Count){$paths[0]}else{$null}
 Compare-S7File 'EndpointServiceBinary' 'camera/S7CameraManage.exe' $manager
 $bindingText=& (Join-Path $Package 'camera/S7CameraManage.exe') transport-status
 if($LASTEXITCODE){throw 'Cannot inspect camera USB driver binding'}
 $binding=$bindingText|ConvertFrom-Json
 $items.Add([pscustomobject]@{Component='CameraTransport';Present=[bool]$binding.present;Deferred=(-not $binding.present);Matches=[bool]($binding.matches -and (-not $binding.present -or $binding.private))})
 foreach($hostService in @(Get-CimInstance Win32_Service -Filter "Name='FrameServer' OR Name='FrameServerMonitor'")){
  if(-not $hostService.ProcessId){continue}
  foreach($module in @((Get-Process -Id $hostService.ProcessId -Module -ErrorAction Stop)|Where-Object {$_.ModuleName -eq 'S7Camera.dll'})){
   Compare-S7File ('LoadedCamera '+$hostService.Name) 'camera/S7Camera.dll' $module.FileName
  }
 }
 if($DeliveryDirectory -and $manager){
  $installedDelivery=Join-Path (Split-Path $manager -Parent) 'delivery'
  foreach($name in @('PackageCommon.ps1','PackageState.ps1','PhonePackage.ps1','Sync-FromPhone.ps1','monitor/DeviceIdentity.ps1','monitor/ReadOnlyDiagnostics.cs')){
   $source=Join-Path $DeliveryDirectory $name;$installed=Join-Path $installedDelivery $name
   $same=$false
   if(Test-Path -LiteralPath $installed -PathType Leaf){
    if((Get-Item -LiteralPath $installed).Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'Delivery helper is a reparse point'}
    if($name.EndsWith('.ps1')){Assert-S7Publisher $installed $PublisherThumbprint}
    $same=(Get-FileHash -LiteralPath $source).Hash -eq (Get-FileHash -LiteralPath $installed).Hash
   }
   $items.Add([pscustomobject]@{Component=('Delivery '+$name);Matches=$same})
  }
 }else{$items.Add([pscustomobject]@{Component='Delivery';Matches=$false})}
 $service=Get-CimInstance Win32_Service -Filter "Name='S7EndpointSync'"
 $owned=$service -and $manager -and $service.PathName -in @(('"'+$manager+'" service'),('"'+$manager+'" service-dev')) -and $service.StartName -eq 'LocalSystem'
 if($service -and -not $owned){throw 'Unowned endpoint service'}
 $items.Add([pscustomobject]@{Component='EndpointService';Matches=[bool]($owned -and $service.StartMode -eq 'Auto' -and $service.PathName -eq ('"'+$manager+'" service'));State=$service.State})
 [pscustomobject]@{Schema='S7_INSTALLED_PACKAGE_STATE_1';Architecture=$arch;Matches=(@($items|Where-Object {-not $_.Matches}).Count -eq 0);Components=$items.ToArray();Changed=$false}
}
