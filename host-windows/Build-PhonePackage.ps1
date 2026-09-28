# Developer packaging only. Signs with the existing approved publisher. No
# trust-store edits, device changes, driver installation or phone writes.
[CmdletBinding()]
param([Parameter(Mandatory)][string]$Package,
 [Parameter(Mandatory)][string]$SetupFile,[Parameter(Mandatory)][string]$Checker,
 [string]$PackageArm64,[string]$SetupFileArm64,[string]$CheckerArm64,
 [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{40}$')][string]$PublisherThumbprint,
 [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{18}$')][string]$DeviceSerial,
 [Parameter(Mandatory)][ValidateRange(1,4294967295)][uint32]$Release,
 [Parameter(Mandatory)][string]$OutputDirectory)
$ErrorActionPreference='Stop'
. (Join-Path $PSScriptRoot 'PackageCommon.ps1')
$out=[IO.Path]::GetFullPath($OutputDirectory)
if(Test-Path -LiteralPath $out){throw 'Use a new output directory'}
$Package=(Resolve-Path -LiteralPath $Package).Path
$candidate=Get-Content -LiteralPath (Join-Path $Package 'package.json') -Raw|ConvertFrom-Json
if($candidate.schema -cne 'S7_WINDOWS_CANDIDATE_2' -or $candidate.camera_backend -cne 'modern' -or $candidate.architecture -notin @('x64','arm64') -or $candidate.device_serial -cne $DeviceSerial.ToLowerInvariant()){throw 'Unsupported package or device serial'}
$multi=[bool]($PackageArm64 -or $SetupFileArm64 -or $CheckerArm64)
if($multi -and (-not $PackageArm64 -or -not $SetupFileArm64 -or -not $CheckerArm64 -or $candidate.architecture -ne 'x64')){throw 'Multi-architecture delivery requires a full x64 and ARM64 pair'}
$prefix=if($multi){'x64/'}else{''}
$cert=Get-Item -LiteralPath ('Cert:\LocalMachine\My\'+$PublisherThumbprint)
if(-not $cert.HasPrivateKey){throw 'Existing signing key unavailable'}
foreach($file in @($SetupFile,$Checker)){
 Assert-S7PEArchitecture $file $candidate.architecture
 Assert-S7Publisher $file $PublisherThumbprint
}
$scripts=@('Install-Package.ps1','PackageCommon.ps1','PackageState.ps1','PhonePackage.ps1','Sync-FromPhone.ps1',
 'monitor/Install-Signed.ps1','monitor/DeviceIdentity.ps1','camera/Install-Local.ps1','camera/Inspect-Runtime.ps1',
 'camera/Backend.ps1','camera/Remove-Local.ps1')
New-Item -ItemType Directory -Path "$out/root"|Out-Null
$root=Join-Path $out 'root'
$files=[Collections.Generic.List[object]]::new()
function Add-S7DeliveryFile([string]$Source,[string]$Relative,[bool]$SignScript=$false){
 $item=Get-Item -LiteralPath $Source
 if($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -or $item.Length -le 0 -or $item.Length -gt 32MB){throw "Invalid delivery file: $Source"}
 $dest=Join-Path $root $Relative;$parent=Split-Path $dest -Parent
 if(-not(Test-Path -LiteralPath $parent)){New-Item -ItemType Directory -Path $parent|Out-Null}
 Copy-Item -LiteralPath $Source -Destination $dest
 if($SignScript){
  $signature=Set-AuthenticodeSignature -LiteralPath $dest -Certificate $cert -HashAlgorithm SHA256
  if($signature.Status -ne 'Valid'){throw "Script signing failed: $Relative"}
 }
 $files.Add([ordered]@{path=$Relative;bytes=(Get-Item -LiteralPath $dest).Length;sha256=(Get-FileHash -LiteralPath $dest).Hash.ToLowerInvariant()})
}
foreach($file in $candidate.files){
 if($file.path -cnotmatch '^(monitor|camera|diagnostics)/[A-Za-z0-9.-]+$' -or $file.sha256 -cnotmatch '^[0-9a-f]{64}$'){throw 'Invalid candidate path/hash'}
 $source=Join-Path $Package $file.path
 if((Get-Item -LiteralPath $source).Length -ne $file.bytes -or (Get-FileHash -LiteralPath $source).Hash -ne $file.sha256){throw 'Candidate changed'}
 if($file.path -match '\.(dll|exe)$'){Assert-S7Publisher $source $PublisherThumbprint}
 Add-S7DeliveryFile $source ($prefix+'package/'+$file.path)
}
Add-S7DeliveryFile (Join-Path $Package 'package.json') ($prefix+'package/package.json')
foreach($script in $scripts){Add-S7DeliveryFile (Join-Path $PSScriptRoot $script) ('host/'+$script) $true}
Add-S7DeliveryFile (Join-Path $PSScriptRoot 'monitor/ReadOnlyDiagnostics.cs') 'host/monitor/ReadOnlyDiagnostics.cs'
Add-S7DeliveryFile $SetupFile ($prefix+'tools/S7Setup.exe')
Add-S7DeliveryFile $Checker ($prefix+'tools/S7PackageCheck.exe')
if($multi){
 $arm=Get-Content -LiteralPath (Join-Path $PackageArm64 'package.json') -Raw|ConvertFrom-Json
 if($arm.schema -cne 'S7_WINDOWS_CANDIDATE_2' -or $arm.camera_backend -cne 'modern' -or $arm.architecture -cne 'arm64' -or $arm.device_serial -cne $DeviceSerial.ToLowerInvariant()){throw 'Unsupported ARM64 package or device serial'}
 foreach($file in $arm.files){
  if($file.path -cnotmatch '^(monitor|camera|diagnostics)/[A-Za-z0-9.-]+$' -or $file.sha256 -cnotmatch '^[0-9a-f]{64}$'){throw 'Invalid ARM64 path/hash'}
  $source=Join-Path $PackageArm64 $file.path
  if((Get-Item -LiteralPath $source).Length -ne $file.bytes -or (Get-FileHash -LiteralPath $source).Hash -ne $file.sha256){throw 'ARM64 candidate changed'}
  if($file.path -match '\.(dll|exe)$'){Assert-S7PEArchitecture $source 'arm64';Assert-S7Publisher $source $PublisherThumbprint}
  Add-S7DeliveryFile $source ('arm64/package/'+$file.path)
 }
 Add-S7DeliveryFile (Join-Path $PackageArm64 'package.json') 'arm64/package/package.json'
 foreach($file in @($SetupFileArm64,$CheckerArm64)){Assert-S7PEArchitecture $file 'arm64';Assert-S7Publisher $file $PublisherThumbprint}
 Add-S7DeliveryFile $SetupFileArm64 'arm64/tools/S7Setup.exe'
 Add-S7DeliveryFile $CheckerArm64 'arm64/tools/S7PackageCheck.exe'
}
$architecture=if($multi){'multi'}else{$candidate.architecture}
$manifest=[ordered]@{schema='S7_SIGNED_PHONE_PACKAGE_1';release=$Release;architecture=$architecture;device_serial=$DeviceSerial.ToLowerInvariant();files=@($files.ToArray()|Sort-Object path)}
$bytes=[Text.UTF8Encoding]::new($false).GetBytes(($manifest|ConvertTo-Json -Depth 6)+"`n")
Add-Type -AssemblyName System.Security
$cms=[Security.Cryptography.Pkcs.SignedCms]::new([Security.Cryptography.Pkcs.ContentInfo]::new($bytes),$true)
$cms.ComputeSignature([Security.Cryptography.Pkcs.CmsSigner]::new($cert),$true)
[IO.File]::WriteAllBytes((Join-Path $root 'bundle.json'),$bytes)
[IO.File]::WriteAllBytes((Join-Path $root 'bundle.p7s'),$cms.Encode())
Add-Type -AssemblyName System.IO.Compression,System.IO.Compression.FileSystem
$archive=Join-Path $out 'package.zip'
$zip=[IO.Compression.ZipFile]::Open($archive,[IO.Compression.ZipArchiveMode]::Create)
try {
 foreach($file in Get-ChildItem -LiteralPath $root -File -Recurse|Sort-Object FullName){
  $name=$file.FullName.Substring($root.Length+1).Replace('\','/')
  [IO.Compression.ZipFileExtensions]::CreateEntryFromFile($zip,$file.FullName,$name,[IO.Compression.CompressionLevel]::Optimal)|Out-Null
 }
}finally{$zip.Dispose()}
if((Get-Item -LiteralPath $archive).Length -gt 128MB){throw 'Phone package exceeds transfer limit'}
$pin=[ordered]@{schema='S7_PHONE_PACKAGE_1';release=$Release;device_serial=$DeviceSerial.ToLowerInvariant();bytes=(Get-Item -LiteralPath $archive).Length;sha256=(Get-FileHash -LiteralPath $archive).Hash.ToLowerInvariant()}
[IO.File]::WriteAllText((Join-Path $out 'PHONE_PACKAGE.json'),($pin|ConvertTo-Json),[Text.UTF8Encoding]::new($false))
$pin|ConvertTo-Json
