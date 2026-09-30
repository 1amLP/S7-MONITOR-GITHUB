[CmdletBinding(SupportsShouldProcess=$true,ConfirmImpact='High')]
param([Parameter(Mandatory)][string]$Package,[string]$DevCon,[string]$SignTool,[switch]$CheckOnly,
 [string]$SetupFile,[string]$PublisherThumbprint,
 [switch]$Upgrade,[AllowEmptyString()][ValidatePattern('^([0-9A-Fa-f]{40})?$')][string]$LocalTestPublisher)
$ErrorActionPreference='Stop'
. (Join-Path $PSScriptRoot 'DeviceIdentity.ps1')
. (Join-Path (Split-Path $PSScriptRoot -Parent) 'PackageCommon.ps1')
$identity=[Security.Principal.WindowsIdentity]::GetCurrent()
if(-not $CheckOnly -and -not ([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)){throw 'Administrator required'}
$Package=(Resolve-Path -LiteralPath $Package).Path
if(-not $SetupFile){$DevCon=(Resolve-Path -LiteralPath $DevCon).Path}
$inf=Join-Path $Package 'S7Monitor.inf';$cat=Join-Path $Package 'S7Monitor.cat';$dll=Join-Path $Package 'S7Monitor.dll'
$nativeArch=Get-S7NativeArchitecture
foreach($path in @($inf,$cat,$dll)){if(-not(Test-Path -LiteralPath $path -PathType Leaf)){throw "Missing $path"}}
Assert-S7PEArchitecture -Path $dll -Architecture $nativeArch
if((Get-AuthenticodeSignature -LiteralPath $cat).Status -ne 'Valid'){throw 'Catalog is not signed by a currently trusted signer. No signing-policy bypass is performed.'}
if($SetupFile){
 $SetupFile=(Resolve-Path -LiteralPath $SetupFile).Path
 Assert-S7PEArchitecture $SetupFile $nativeArch
 Assert-S7Publisher $SetupFile $PublisherThumbprint
}else{
Assert-S7PEArchitecture -Path $DevCon -Architecture $nativeArch
$devSignature=Get-AuthenticodeSignature -LiteralPath $DevCon
if($devSignature.Status -ne 'Valid' -or $devSignature.SignerCertificate.Subject -notmatch 'O=Microsoft Corporation'){throw 'Use the signed Microsoft WDK DevCon executable'}
if($SignTool){
 $verify=Get-Item -LiteralPath (Resolve-Path -LiteralPath $SignTool).Path -ErrorAction Stop
 $verify=[pscustomobject]@{Path=$verify.FullName}
}else{$verify=Get-Command signtool.exe -CommandType Application -ErrorAction Stop}
Assert-S7PEArchitecture -Path $verify.Path -Architecture $nativeArch
$toolSignature=Get-AuthenticodeSignature -LiteralPath $verify.Path
if($toolSignature.Status -ne 'Valid' -or $toolSignature.SignerCertificate.Subject -notmatch 'O=Microsoft Corporation'){throw 'Trusted Microsoft SignTool required'}
}
$policy='/kp'
if($LocalTestPublisher){
 # Explicit, already-trusted local UMDF publisher only. Never add trust or change boot policy.
 Assert-S7Publisher -Path $cat -Thumbprint $LocalTestPublisher
 Assert-S7Publisher -Path $dll -Thumbprint $LocalTestPublisher
 $policy='/pa'
}
foreach($path in @($inf,$dll)){
    if($SetupFile){& $SetupFile catalog-check $cat $path}
    else{& $verify.Path verify $policy /v /c $cat $path}
    if($LASTEXITCODE -ne 0){throw "Catalog membership/signature rejected: $path"}
}
$existing=@(Get-S7MonitorDevice)
if($existing.Count -gt 1){throw 'Ambiguous S7 monitor ownership'}
if($existing.Count -and -not $Upgrade){throw 'Existing S7 monitor requires explicit Upgrade'}
if($existing.Count){
 $old=Get-S7MonitorDriverFile $existing[0].InstanceId
 $publisher=(Get-AuthenticodeSignature -LiteralPath $cat).SignerCertificate.Thumbprint
 Assert-S7Publisher -Path $old -Thumbprint $publisher
 $versions=[regex]::Matches([IO.File]::ReadAllText($inf),'(?m)^DriverVer=[^,]+,(\d+(?:\.\d+){3})\s*$')
 if($versions.Count -ne 1){throw 'Missing unique monitor DriverVer'}
 $installedVersion=[version](Get-PnpDeviceProperty -InstanceId $existing[0].InstanceId -KeyName DEVPKEY_Device_DriverVersion).Data
 if($installedVersion -eq [version]$versions[0].Groups[1].Value -and (Get-FileHash -LiteralPath $old).Hash -ne (Get-FileHash -LiteralPath $dll).Hash){
  throw 'Changed monitor binary has the installed DriverVer; rebuild with a new version before updating'
 }
}
if($CheckOnly){Write-Output 'Monitor installation checks passed; no device changed.';return}
if($PSCmdlet.ShouldProcess('Root\S7H264Monitor','Install or update one signed S7 H.264 virtual monitor')){
    $verb=if($existing.Count){'update'}else{'install'}
    if($SetupFile){& $SetupFile monitor-install $inf}
    else{& $DevCon $verb $inf 'Root\S7H264Monitor'}
    if($LASTEXITCODE -notin @(0,1)){throw "S7 monitor installation failed: $LASTEXITCODE"}
    $result=$LASTEXITCODE
    $devices=@(Get-S7MonitorDevice)
    if($devices.Count -ne 1){throw 'S7 monitor count changed during installation'}
    $installed=Get-S7MonitorDriverFile $devices[0].InstanceId
    if((Get-FileHash -LiteralPath $installed).Hash -ne (Get-FileHash -LiteralPath $dll).Hash){throw 'Installed monitor binary differs from the package'}
    Get-S7MonitorDevice | Format-List Status,FriendlyName,InstanceId
    $global:LASTEXITCODE=$result
}
# This does not alter DisplayLink/graphics drivers, Secure Boot, USB controller,
# camera/microphone permissions, network or the phone. Keep the printed instance ID.
