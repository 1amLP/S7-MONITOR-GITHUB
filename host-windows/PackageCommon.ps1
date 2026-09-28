# Shared read-only preflight. No certificate installation, registry writes or signing.
function Assert-S7RegularPath([string]$Path) {
 $part=Get-Item -LiteralPath $Path -ErrorAction Stop
 if($part.PSIsContainer -or $part.Length -le 0 -or $part.Length -gt 256MB){throw "Invalid payload: $Path"}
 while($part){
  if($part.Attributes -band [IO.FileAttributes]::ReparsePoint){throw "Reparse path refused: $Path"}
  $parent=Split-Path $part.FullName -Parent
  if(-not $parent -or $parent -eq $part.FullName){break}
  $part=Get-Item -LiteralPath $parent -ErrorAction Stop
 }
}
function Get-S7MachineInfo {
    if(-not ('S7NativeMachine' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class S7NativeMachine {
 [DllImport("kernel32.dll",SetLastError=true)]
 public static extern bool IsWow64Process2(IntPtr process,out ushort processMachine,out ushort nativeMachine);
 [DllImport("kernel32.dll")] public static extern IntPtr GetCurrentProcess();
}
'@
    }
    [UInt16]$process=0; [UInt16]$native=0
    if(-not [S7NativeMachine]::IsWow64Process2([S7NativeMachine]::GetCurrentProcess(),[ref]$process,[ref]$native)) {
        throw "Native machine query failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
    }
    if($native -notin @(0x8664,0xaa64)){throw 'Only native x64 and ARM64 Windows are supported by these build targets'}
    [pscustomobject]@{Process=$process;Native=$native}
}
function Get-S7NativeArchitecture {
    $machine=Get-S7MachineInfo
    if($machine.Native -eq 0xaa64){'arm64'}else{'x64'}
}
function Assert-S7NativePowerShell([string]$Architecture) {
    $machine=Get-S7MachineInfo
    if($machine.Process -ne 0){throw 'Use native 64-bit PowerShell, not x86/x64 emulation; camera COM must use the native registry view'}
    $expected=if($Architecture -eq 'arm64'){0xaa64}else{0x8664}
    if($machine.Native -ne $expected){throw 'PowerShell/package architecture mismatch'}
}
function Assert-S7PEArchitecture([string]$Path,[ValidateSet('x64','arm64')][string]$Architecture) {
    $item=Get-Item -LiteralPath $Path -ErrorAction Stop
    if($item.Attributes -band [IO.FileAttributes]::ReparsePoint){throw "Reparse point refused: $Path"}
    $stream=[IO.File]::OpenRead($item.FullName)
    $reader=New-Object IO.BinaryReader($stream)
    try {
        if($stream.Length -lt 64 -or $reader.ReadUInt16() -ne 0x5a4d){throw "Invalid DOS header: $Path"}
        $stream.Position=60; $offset=$reader.ReadUInt32()
        if($offset -lt 64 -or $offset -gt $stream.Length-26){throw "Invalid PE extent: $Path"}
        $stream.Position=$offset
        if($reader.ReadUInt32() -ne 0x4550){throw "Invalid PE signature: $Path"}
        $machine=$reader.ReadUInt16()
        $expected=if($Architecture -eq 'arm64'){0xaa64}else{0x8664}
        if($machine -ne $expected){throw "Wrong PE architecture: $Path"}
        $stream.Position=$offset+24
        if($reader.ReadUInt16() -ne 0x20b){throw "PE32+ required: $Path"}
    } finally {$reader.Dispose()}
}
function Assert-S7Publisher([string]$Path,[ValidatePattern('^[0-9A-Fa-f]{40}$')][string]$Thumbprint) {
    $signature=Get-AuthenticodeSignature -LiteralPath $Path
    if($signature.Status -ne 'Valid' -or $null -eq $signature.SignerCertificate -or $signature.SignerCertificate.Thumbprint -ne $Thumbprint){
        throw "Trusted signature from the explicitly approved publisher required: $Path"
    }
}
