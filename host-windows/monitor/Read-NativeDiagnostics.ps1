[CmdletBinding()]
param([Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{18}$')][string]$DeviceSerial,[string]$OutputPath)
$ErrorActionPreference = 'Stop'
if (-not ('S7ReadOnlyDiagnostics' -as [type])) {
    Add-Type -Path (Join-Path $PSScriptRoot 'ReadOnlyDiagnostics.cs')
}
[S7ReadOnlyDiagnostics]::ExpectedSerial = $DeviceSerial
try {
    $json = [S7ReadOnlyDiagnostics]::Read()
    $first = $json | ConvertFrom-Json
    if ($first.PSObject.Properties.Name -contains 'diagnostic_revision') {
        $deadline = [DateTime]::UtcNow.AddSeconds(2)
        do {
            Start-Sleep -Milliseconds 100
            $json = [S7ReadOnlyDiagnostics]::Read()
            $fresh = $json | ConvertFrom-Json
            if ($fresh.diagnostic_revision -gt $first.diagnostic_revision) { break }
        } while ([DateTime]::UtcNow -lt $deadline)
        if ($fresh.diagnostic_revision -le $first.diagnostic_revision) { throw 'S7 diagnostic producer did not publish a fresh snapshot' }
    }
}
catch {
    if ($OutputPath) {
        $errorPath = [IO.Path]::GetFullPath($OutputPath) + '.error.txt'
        if (-not (Test-Path -LiteralPath $errorPath)) {
            $details = $_.Exception.ToString()
            if ($_.Exception.InnerException -is [ComponentModel.Win32Exception]) {
                $details += "`nWin32=" + $_.Exception.InnerException.NativeErrorCode
            }
            [IO.File]::WriteAllText($errorPath, $details)
        }
    }
    throw
}
$report = $json | ConvertFrom-Json
if ($OutputPath) {
    $target = [IO.Path]::GetFullPath($OutputPath)
    if (Test-Path -LiteralPath $target) { throw "Output exists: $target" }
    [IO.File]::WriteAllText($target, $json, [Text.UTF8Encoding]::new($false))
}
$report
