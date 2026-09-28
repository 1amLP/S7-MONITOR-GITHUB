[CmdletBinding()]
param([Parameter(Mandatory)][string]$OutputFile,[Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{40}$')][string]$PublisherThumbprint,[switch]$ReleaseStaleSource,[switch]$ReleaseLoadedSource)
$ErrorActionPreference='Stop'
$report=[ordered]@{Success=$false;Error='';Released=$false;Processes=@()}
try {
    $identity=[Security.Principal.WindowsIdentity]::GetCurrent()
    if(-not([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)){throw 'Administrator required'}
    $class='HKLM:\SOFTWARE\Classes\CLSID\{4C850E86-1698-44BF-893E-43ECF61BAAA9}\InprocServer32'
    $registered=(Get-Item -LiteralPath $class).GetValue('')
    $report.RegisteredDll=$registered
    $front='HKLM:\SOFTWARE\Classes\CLSID\{14986CDD-1DA2-4A72-B953-914E65934921}\InprocServer32'
    if(Test-Path -LiteralPath $front){
        $report.FrontRegisteredDll=(Get-Item -LiteralPath $front).GetValue('')
        if($report.FrontRegisteredDll -ne $registered){throw 'Front and rear source registrations differ'}
    }else{$report.FrontRegisteredDll=$null}
    $root=[IO.Path]::GetFullPath((Join-Path $env:ProgramFiles 'S7 Appliance\Camera'))+'\'
    if(-not [IO.Path]::GetFullPath($registered).StartsWith($root,[StringComparison]::OrdinalIgnoreCase)){throw 'Unexpected registered source path'}
    $signature=Get-AuthenticodeSignature -LiteralPath $registered
    if($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Thumbprint -ne $PublisherThumbprint){throw 'Unapproved source signature'}
    $registeredHash=(Get-FileHash -LiteralPath $registered -Algorithm SHA256).Hash
    $report.RegisteredSHA256=$registeredHash
    $report.EquivalentLoadedPaths=@()
    $stale=$false
    $modules=@()
    foreach($service in @(Get-CimInstance Win32_Service -Filter "Name='FrameServer' OR Name='FrameServerMonitor'")){
    $report.ServiceProcess=$service.ProcessId
    if($service.ProcessId){
        $modules=@((Get-Process -Id $service.ProcessId -Module)|Where-Object{$_.ModuleName -eq 'S7Camera.dll'}|ForEach-Object{$_.FileName})
        $report.Processes+=@{Id=$service.ProcessId;Name=$service.Name;Modules=$modules}
        foreach($path in $modules){
            if(-not [IO.Path]::GetFullPath($path).StartsWith($root,[StringComparison]::OrdinalIgnoreCase)){throw 'Unexpected loaded source path'}
            if($path -ne $registered){
                $file=Get-Item -LiteralPath $path
                if($file.PSIsContainer -or ($file.Attributes -band [IO.FileAttributes]::ReparsePoint)){throw 'Loaded source is not a regular owned file'}
                $loadedSignature=Get-AuthenticodeSignature -LiteralPath $path
                if($loadedSignature.Status -ne 'Valid' -or $loadedSignature.SignerCertificate.Thumbprint -ne $PublisherThumbprint){throw 'Unapproved loaded source signature'}
                if((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash -ne $registeredHash){$stale=$true}
                else{$report.EquivalentLoadedPaths+=$path}
            }
        }
    }
    }
    $report.Stale=$stale
    $loaded=@($report.Processes|Where-Object {$_.Modules.Count})
    if(($ReleaseStaleSource -and $stale) -or ($ReleaseLoadedSource -and $loaded.Count)){
        if(Get-Process WindowsCamera -ErrorAction SilentlyContinue){throw 'Close Windows Camera before releasing its source'}
        throw 'S7Camera.dll is still loaded. Close camera clients and retry; shared FrameServer will not be stopped. Restart Windows if its module remains loaded.'
    }
    $report.Success=$true
}catch{$report.Error=$_.Exception.Message}
$report|ConvertTo-Json -Depth 6|Set-Content -LiteralPath $OutputFile -Encoding UTF8
$report|ConvertTo-Json -Depth 6
if(-not $report.Success){exit 1}
