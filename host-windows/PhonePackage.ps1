# A phone payload is data until its detached publisher signature is validated.
function Get-S7SignedBundleManifest([byte[]]$ManifestBytes,[byte[]]$SignatureBytes,[string]$PublisherThumbprint) {
 Add-Type -AssemblyName System.Security
 if($ManifestBytes.Length -gt 64KB -or $SignatureBytes.Length -gt 64KB){throw 'Bundle metadata limit exceeded'}
 $cms=[Security.Cryptography.Pkcs.SignedCms]::new([Security.Cryptography.Pkcs.ContentInfo]::new($ManifestBytes),$true)
 $cms.Decode($SignatureBytes)
 if($cms.SignerInfos.Count -ne 1 -or $cms.SignerInfos[0].Certificate.Thumbprint -ne $PublisherThumbprint){throw 'Unexpected delivery publisher'}
 $cms.CheckSignature($false)
 $manifest=[Text.Encoding]::UTF8.GetString($ManifestBytes)|ConvertFrom-Json
 if($manifest.schema -cne 'S7_SIGNED_PHONE_PACKAGE_1' -or $manifest.release -le 0 -or $manifest.architecture -notin @('x64','arm64','multi') -or $manifest.device_serial -cnotmatch '^[0-9a-f]{18}$'){throw 'Unsupported signed manifest'}
 return $manifest
}

function Expand-S7PhonePackage {
 [CmdletBinding()]
 param([Parameter(Mandatory)][string]$Archive,[Parameter(Mandatory)][string]$Destination,
       [Parameter(Mandatory)][uint32]$Release,
       [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{40}$')][string]$PublisherThumbprint)
 Add-Type -AssemblyName System.IO.Compression,System.IO.Compression.FileSystem
 Add-Type -AssemblyName System.Security
 if(Test-Path -LiteralPath $Destination){throw 'Use a new package destination'}
 $zip=[IO.Compression.ZipFile]::OpenRead($Archive)
 try {
  $entries=@{};$total=0L
  foreach($entry in $zip.Entries){
   $name=$entry.FullName
   if($name -cnotmatch '^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*$' -or @($name.Split('/')|Where-Object {$_ -in @('.','..') -or $_.EndsWith('.')}).Count -or $entries.ContainsKey($name)){throw 'Invalid or duplicate archive path'}
   if($entry.Length -le 0 -or $entry.Length -gt 32MB){throw 'Archive member size rejected'}
   $total+=$entry.Length
   if($total -gt 256MB -or $entries.Count -ge 64){throw 'Archive bounds exceeded'}
   $entries[$name]=$entry
  }
  if(-not $entries.ContainsKey('bundle.json') -or -not $entries.ContainsKey('bundle.p7s')){throw 'Signed bundle manifest missing'}
  function Read-S7ZipEntry($Entry,[long]$Limit) {
   if($Entry.Length -gt $Limit){throw 'Metadata limit exceeded'}
   $stream=$Entry.Open();$memory=[IO.MemoryStream]::new()
   try{
    $buffer=New-Object byte[] 8192
    while(($n=$stream.Read($buffer,0,$buffer.Length)) -gt 0){
     if($memory.Length+$n -gt $Limit){throw 'Expanded metadata exceeded limit'}
     $memory.Write($buffer,0,$n)
    }
    if($memory.Length -ne $Entry.Length){throw 'Truncated metadata'};return ,$memory.ToArray()
   }
   finally{$stream.Dispose();$memory.Dispose()}
  }
  $manifestBytes=Read-S7ZipEntry $entries['bundle.json'] 64KB
  $signatureBytes=Read-S7ZipEntry $entries['bundle.p7s'] 64KB
  $manifest=Get-S7SignedBundleManifest $manifestBytes $signatureBytes $PublisherThumbprint
  if($manifest.release -ne $Release){throw 'Delivery release differs from S7'}
  $listed=@{}
  foreach($file in $manifest.files){
   if($file.path -in @('bundle.json','bundle.p7s') -or $listed.ContainsKey($file.path) -or -not $entries.ContainsKey($file.path) -or $file.sha256 -cnotmatch '^[0-9a-f]{64}$' -or $entries[$file.path].Length -ne $file.bytes){throw 'Manifest/archive mismatch'}
   $listed[$file.path]=$file
  }
  if($listed.Count+2 -ne $entries.Count){throw 'Unlisted archive contents'}
  New-Item -ItemType Directory -Path $Destination|Out-Null
  $root=[IO.Path]::GetFullPath($Destination)+[IO.Path]::DirectorySeparatorChar
  foreach($name in $entries.Keys){
   $path=[IO.Path]::GetFullPath((Join-Path $root $name))
   if(-not $path.StartsWith($root,[StringComparison]::OrdinalIgnoreCase)){throw 'Archive path escaped destination'}
   $parent=Split-Path $path -Parent
   if(-not(Test-Path -LiteralPath $parent)){New-Item -ItemType Directory -Path $parent|Out-Null}
   $inputStream=$entries[$name].Open();$output=[IO.File]::Open($path,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
   try{
    $buffer=New-Object byte[] 65536
    while(($n=$inputStream.Read($buffer,0,$buffer.Length)) -gt 0){
     if($output.Length+$n -gt $entries[$name].Length){throw 'Expanded member exceeded signed size'}
     $output.Write($buffer,0,$n)
    }
   }finally{$inputStream.Dispose();$output.Dispose()}
   if((Get-Item -LiteralPath $path).Length -ne $entries[$name].Length){throw 'Extraction length mismatch'}
   if($listed.ContainsKey($name) -and (Get-FileHash -LiteralPath $path).Hash -ne $listed[$name].sha256){throw 'Extracted hash mismatch'}
  }
  return $manifest
 }finally{$zip.Dispose()}
}

function Assert-S7ExpandedPackage([string]$Directory,[uint32]$Release,[string]$PublisherThumbprint) {
 $root=[IO.Path]::GetFullPath($Directory)+[IO.Path]::DirectorySeparatorChar
 foreach($name in @('bundle.json','bundle.p7s')){
  $file=Get-Item -LiteralPath (Join-Path $root $name)
  if($file.PSIsContainer -or $file.Length -gt 64KB -or ($file.Attributes -band [IO.FileAttributes]::ReparsePoint)){throw 'Invalid cached metadata'}
 }
 $meta=Get-S7SignedBundleManifest ([IO.File]::ReadAllBytes((Join-Path $root 'bundle.json'))) ([IO.File]::ReadAllBytes((Join-Path $root 'bundle.p7s'))) $PublisherThumbprint
 if($meta.release -ne $Release){throw 'Cached release mismatch'}
 $seen=@{}
 foreach($entry in $meta.files){
  if($entry.path -cnotmatch '^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*$' -or @($entry.path.Split('/')|Where-Object {$_ -in @('.','..') -or $_.EndsWith('.')}).Count -or $seen.ContainsKey($entry.path)){throw 'Invalid cached manifest path'}
  $seen[$entry.path]=$true
  $path=[IO.Path]::GetFullPath((Join-Path $root $entry.path))
  if(-not $path.StartsWith($root,[StringComparison]::OrdinalIgnoreCase)){throw 'Cached path escaped root'}
  $part=Get-Item -LiteralPath $path
  while($part.FullName.Length -ge $root.Length){
   if($part.Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'Cached reparse point'}
   $part=Get-Item -LiteralPath (Split-Path $part.FullName -Parent)
  }
  $file=Get-Item -LiteralPath $path
  if($file.PSIsContainer -or $file.Length -ne $entry.bytes -or $entry.sha256 -cnotmatch '^[0-9a-f]{64}$' -or (Get-FileHash -LiteralPath $path).Hash -ne $entry.sha256){throw 'Cached content changed'}
 }
 return $meta
}
