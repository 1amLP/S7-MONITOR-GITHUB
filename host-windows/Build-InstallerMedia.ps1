[CmdletBinding()]
param([Parameter(Mandatory)][string]$PhonePackageDirectory,
 [Parameter(Mandatory)][string]$SetupFile,
 [Parameter(Mandatory)][string]$OutputDirectory,
 [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{40}$')][string]$PublisherThumbprint)
$ErrorActionPreference='Stop'
. "$PSScriptRoot/PackageCommon.ps1"
$out=[IO.Path]::GetFullPath($OutputDirectory)
if(Test-Path -LiteralPath $out){throw 'Use a new media directory'}
Assert-S7Publisher $SetupFile $PublisherThumbprint
$cert=Get-Item -LiteralPath ('Cert:\LocalMachine\My\'+$PublisherThumbprint)
if(-not $cert.HasPrivateKey){throw 'Existing signing key unavailable'}
New-Item -ItemType Directory -Path $out|Out-Null
Copy-Item -LiteralPath $SetupFile -Destination "$out/S7Setup.exe"
foreach($name in @('Install-FromMedia.ps1','PackageCommon.ps1','PhonePackage.ps1')){
 Copy-Item -LiteralPath (Join-Path $PSScriptRoot $name) -Destination (Join-Path $out $name)
 $signature=Set-AuthenticodeSignature -LiteralPath (Join-Path $out $name) -Certificate $cert -HashAlgorithm SHA256
 if($signature.Status -ne 'Valid'){throw "Cannot sign installer entry: $name"}
}
foreach($name in @('package.zip','PHONE_PACKAGE.json')){Copy-Item -LiteralPath (Join-Path $PhonePackageDirectory $name) -Destination (Join-Path $out $name)}
[IO.File]::WriteAllText((Join-Path $out 'autorun.inf'),"[autorun]`r`nopen=S7Setup.exe`r`naction=Install S7 drivers`r`nlabel=S7 SETUP`r`nicon=S7Setup.exe`r`n",[Text.Encoding]::ASCII)
Write-Output 'Installer media files prepared. AutoPlay may require opening S7Setup.exe; no policy is changed.'
