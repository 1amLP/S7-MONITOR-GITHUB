# Produces a driver-publishing candidate. It does not install/prestage it.
[CmdletBinding()]
param([Parameter(Mandatory)][string]$BootstrapExe,
 [Parameter(Mandatory)][string]$DeliveryRoot,
 [Parameter(Mandatory)][string]$OutputDirectory,
 [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{40}$')][string]$PublisherThumbprint,
 [ValidateSet('x64','arm64')][string]$Architecture='x64')
$ErrorActionPreference='Stop'
. (Join-Path (Split-Path $PSScriptRoot -Parent) 'PackageCommon.ps1')
. (Join-Path (Split-Path $PSScriptRoot -Parent) 'PhonePackage.ps1')
$out=[IO.Path]::GetFullPath($OutputDirectory)
if(Test-Path -LiteralPath $out){throw 'Use a new output directory'}
Assert-S7PEArchitecture $BootstrapExe $Architecture
Assert-S7Publisher $BootstrapExe $PublisherThumbprint
$meta=Get-S7SignedBundleManifest ([IO.File]::ReadAllBytes((Join-Path $DeliveryRoot 'bundle.json'))) ([IO.File]::ReadAllBytes((Join-Path $DeliveryRoot 'bundle.p7s'))) $PublisherThumbprint
if($meta.architecture -ne $Architecture -and $meta.architecture -ne 'multi'){throw 'Bootstrap and delivery architectures differ'}
New-Item -ItemType Directory -Path "$out/delivery/monitor"|Out-Null
Copy-Item -LiteralPath $BootstrapExe -Destination "$out/S7PackageBootstrap.exe"
foreach($name in @('PackageCommon.ps1','PackageState.ps1','PhonePackage.ps1','Sync-FromPhone.ps1','monitor/DeviceIdentity.ps1','monitor/ReadOnlyDiagnostics.cs')){
 $file=Join-Path "$DeliveryRoot/host" $name
 $pin=@($meta.files|Where-Object {$_.path -ceq ('host/'+$name)})
 if($pin.Count -ne 1 -or (Get-FileHash -LiteralPath $file).Hash -ne $pin[0].sha256){throw 'Bootstrap helper differs from signed delivery'}
 if($name.EndsWith('.ps1')){Assert-S7Publisher $file $PublisherThumbprint}
 Copy-Item -LiteralPath $file -Destination (Join-Path "$out/delivery" $name)
}
foreach($name in @('bundle.json','bundle.p7s')){Copy-Item -LiteralPath (Join-Path $DeliveryRoot $name) -Destination (Join-Path "$out/delivery" $name)}
$inf=Get-Content -LiteralPath "$PSScriptRoot/S7Bootstrap.inf" -Raw
if($Architecture -eq 'arm64'){$inf=$inf.Replace('NTamd64','NTarm64')}
[IO.File]::WriteAllText((Join-Path $out 'S7Bootstrap.inf'),$inf,[Text.Encoding]::Unicode)
[ordered]@{Schema='S7_BOOTSTRAP_CANDIDATE_1';Architecture=$Architecture;Installed=$false;CatalogSigned=$false;PublishedToWindowsUpdate=$false;AutomaticCleanPCInstallTested=$false}|ConvertTo-Json|Set-Content -LiteralPath "$out/build-state.json"
