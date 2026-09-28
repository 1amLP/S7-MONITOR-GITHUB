[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{18}$')][string]$DeviceSerial,
    [ValidateSet('Capture','Open','Close','Back','Tap','Scroll','Recents','Recovery')][string]$Action = 'Capture',
    [int]$X = 0, [int]$Y = 0, [uint32]$Capture = 0,
    [string]$OutputPath,
    [switch]$ConfirmDeviceRecovery
)
$ErrorActionPreference = 'Stop'
if (-not ('S7ReadOnlyDiagnostics' -as [type])) {
    Add-Type -Path (Join-Path $PSScriptRoot 'ReadOnlyDiagnostics.cs')
}
[S7ReadOnlyDiagnostics]::ExpectedSerial = $DeviceSerial
if ($Action -eq 'Capture' -and -not $OutputPath) { throw 'Capture requires a new PNG output path' }
if ($OutputPath -and (Test-Path -LiteralPath $OutputPath)) { throw "Output exists: $OutputPath" }
if ($Action -eq 'Recovery') {
    if (-not $ConfirmDeviceRecovery) { throw 'Recovery requires -ConfirmDeviceRecovery' }
    $X = 0x52564352
}
$kind = @{Capture=1;Open=2;Close=3;Back=4;Tap=5;Scroll=6;Recents=7;Recovery=8}[$Action]
$packet = [S7ReadOnlyDiagnostics]::Menu($kind, $X, $Y, $Capture)
$metaSize = [BitConverter]::ToUInt32($packet, 12)
$metadata = [Text.Encoding]::UTF8.GetString($packet, 32, $metaSize) | ConvertFrom-Json
$metadata | Add-Member NoteProperty sequence ([BitConverter]::ToUInt32($packet, 4))
if ([BitConverter]::ToUInt32($packet, 8) -eq 3) { throw $metadata.error }
if ($Action -eq 'Capture') {
    . (Join-Path $PSScriptRoot 'Save-NativeCapture.ps1')
    Save-NativeCapture -Packet $packet -Path $OutputPath
    $metadata | Add-Member NoteProperty image ([IO.Path]::GetFullPath($OutputPath))
}
$metadata
