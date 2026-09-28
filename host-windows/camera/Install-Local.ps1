[CmdletBinding(SupportsShouldProcess=$true,ConfirmImpact='High')]
param([Parameter(Mandatory)][string]$BuildDirectory,
    [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{40}$')][string]$PublisherThumbprint,
    [switch]$Upgrade,[switch]$CaptureTest,[string]$LogDirectory,
    [switch]$RestartCameraService,[string]$DeliveryDirectory,[switch]$DevelopmentHostPackage)
$ErrorActionPreference='Stop'
. (Join-Path (Split-Path $PSScriptRoot -Parent) 'PackageCommon.ps1')
. (Join-Path $PSScriptRoot 'Backend.ps1')
$cameraMutex=$null;$cameraLock=$false
$PublisherThumbprint=$PublisherThumbprint.ToUpperInvariant()
$BuildDirectory=(Resolve-Path -LiteralPath $BuildDirectory).Path
$receipt=[ordered]@{Success=$false;Stage='preflight';Error='';Directory='';ClassId='{4C850E86-1698-44BF-893E-43ECF61BAAA9}'}
if(-not $LogDirectory){$LogDirectory=Join-Path ([IO.Path]::GetTempPath()) ('s7-camera-install-'+[Guid]::NewGuid().ToString('N'))}
$LogDirectory=[IO.Path]::GetFullPath($LogDirectory)
if($LogDirectory -eq $BuildDirectory -or $LogDirectory.StartsWith($BuildDirectory+[IO.Path]::DirectorySeparatorChar,[StringComparison]::OrdinalIgnoreCase)){throw 'LogDirectory must be outside the camera payload'}
if(Test-Path -LiteralPath $LogDirectory){throw 'Use a new LogDirectory'}
$result=Join-Path $LogDirectory 'install-result.json'
$writeReceipt=$true
$class="HKLM:\SOFTWARE\Classes\CLSID\$($receipt.ClassId)"
$frontClass='HKLM:\SOFTWARE\Classes\CLSID\{14986CDD-1DA2-4A72-B953-914E65934921}'
$previousFrontDll=$null
$restartServices=@()
$previousEndpointService=$null
$endpointServiceChanged=$false
$endpointServiceStopped=$false
$deliveryFiles=@('PackageCommon.ps1','PackageState.ps1','PhonePackage.ps1','Sync-FromPhone.ps1','monitor/DeviceIdentity.ps1','monitor/ReadOnlyDiagnostics.cs')
$deliveryHashes=@{}

$registered=$false
$mutationStarted=$false
$oldDll=$null
$sourceUnchanged=$false
function Release-LoadedSource([string]$Name) {
    $runtime=Join-Path $LogDirectory $Name
    & "$PSScriptRoot/Inspect-Runtime.ps1" -OutputFile $runtime -PublisherThumbprint $PublisherThumbprint -ReleaseLoadedSource
    $inspection=Get-Content -LiteralPath $runtime -Raw|ConvertFrom-Json
    if(-not $inspection.Success){throw "Cannot release previous camera runtime: $($inspection.Error)"}
}
function Set-S7EndpointServicePath([string]$Command) {
    $service=Get-CimInstance Win32_Service -Filter "Name='S7EndpointSync'"
    if(-not $service -or $service.StartName -ne 'LocalSystem'){throw 'Owned endpoint service is unavailable'}
    $changed=Invoke-CimMethod -InputObject $service -MethodName Change -Arguments @{PathName=$Command;StartMode='Automatic'}
    if($changed.ReturnValue -ne 0){throw "Endpoint service Change failed: $($changed.ReturnValue)"}
    $actual=Get-CimInstance Win32_Service -Filter "Name='S7EndpointSync'"
    if($actual.PathName -cne $Command -or $actual.StartMode -ne 'Auto'){throw 'Endpoint service path readback differs'}
}
try {
    Write-Output 'S7 Camera: checking the signed package and camera registrations'
    if(Test-Path -LiteralPath (Join-Path $BuildDirectory 'cancel-install')){throw 'This installation attempt was cancelled before deployment'}
    $identity=[Security.Principal.WindowsIdentity]::GetCurrent()
    if(-not([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)){throw 'Administrator required'}
    if([Environment]::OSVersion.Version.Build -lt 22000){throw 'This compatibility camera currently requires Windows 11'}
    $nativeArch=Get-S7NativeArchitecture
    Assert-S7NativePowerShell -Architecture $nativeArch
    Assert-S7CameraBackend 'modern'
    if(Get-Process WindowsCamera -ErrorAction SilentlyContinue){throw 'Close Windows Camera before installing its source'}
    if(Test-Path -LiteralPath $class){
        if(-not $Upgrade){throw 'S7 camera COM class already exists; explicit Upgrade required'}
        $oldDll=(Get-Item -LiteralPath "$class\InprocServer32").GetValue('')
        $allowed=[IO.Path]::GetFullPath((Join-Path $env:ProgramFiles 'S7 Appliance\Camera'))+'\'
        if(-not [IO.Path]::GetFullPath($oldDll).StartsWith($allowed,[StringComparison]::OrdinalIgnoreCase)){throw 'Existing COM path is not owned by S7 Camera'}
        $signature=Get-AuthenticodeSignature -LiteralPath $oldDll
        if($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Thumbprint -ne $PublisherThumbprint){throw 'Existing source signature is not approved'}
        $receipt.PreviousDll=$oldDll
        $oldManager=Join-Path (Split-Path $oldDll -Parent) 'S7CameraManage.exe'
        $managerSignature=Get-AuthenticodeSignature -LiteralPath $oldManager
        if($managerSignature.Status -ne 'Valid' -or $managerSignature.SignerCertificate.Thumbprint -ne $PublisherThumbprint){throw 'Previous camera manager signature is not approved'}
    }
    if(Test-Path -LiteralPath $frontClass){
        if(-not $Upgrade -or -not $oldDll){throw 'Existing front camera requires an owned paired Upgrade'}
        $previousFrontDll=(Get-Item -LiteralPath "$frontClass\InprocServer32").GetValue('')
        if($previousFrontDll -ne $oldDll){throw 'Front and rear COM registrations do not match; refusing overwrite'}
    }
    $inputHashes=@{}
    foreach($name in @('S7Camera.dll','S7CameraManage.exe')){
        $file=Join-Path $BuildDirectory $name
        Assert-S7PEArchitecture -Path $file -Architecture $nativeArch
        Assert-S7Publisher -Path $file -Thumbprint $PublisherThumbprint
        $inputHashes[$name]=(Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash
    }
    if($DeliveryDirectory){
        $DeliveryDirectory=(Resolve-Path -LiteralPath $DeliveryDirectory).Path
        . (Join-Path (Split-Path $PSScriptRoot -Parent) 'PhonePackage.ps1')
        $deliveryRoot=Split-Path $DeliveryDirectory -Parent
        $signedDelivery=Get-S7SignedBundleManifest ([IO.File]::ReadAllBytes((Join-Path $deliveryRoot 'bundle.json'))) ([IO.File]::ReadAllBytes((Join-Path $deliveryRoot 'bundle.p7s'))) $PublisherThumbprint
        foreach($name in $deliveryFiles){
            $path=Join-Path $DeliveryDirectory $name
            $file=Get-Item -LiteralPath $path
            if($file.PSIsContainer -or ($file.Attributes -band [IO.FileAttributes]::ReparsePoint)){throw 'Invalid delivery helper'}
            if($name.EndsWith('.ps1')){Assert-S7Publisher $path $PublisherThumbprint}
            $deliveryHashes[$name]=(Get-FileHash -LiteralPath $path).Hash
            $entry=@($signedDelivery.files|Where-Object {$_.path -ceq ('host/'+$name)})
            if($entry.Count -ne 1 -or $entry[0].bytes -ne $file.Length -or $entry[0].sha256 -ne $deliveryHashes[$name]){throw 'Helper differs from signed delivery'}
        }
    }
    $previousEndpointService=Get-CimInstance Win32_Service -Filter "Name='S7EndpointSync'"
    if($previousEndpointService){
        if(-not $oldDll -or $previousEndpointService.PathName -notin @(('"'+$oldManager+'" service'),('"'+$oldManager+'" service-dev')) -or $previousEndpointService.StartName -ne 'LocalSystem'){
            throw 'Existing endpoint service is not owned by the approved S7 package'
        }
    }
    if(-not $PSCmdlet.ShouldProcess('S7 Rear and Front COM sources','Install pre-signed camera package')){$writeReceipt=$false;return}
    $cameraMutex=New-Object Threading.Mutex($false,'Global\S7NativeCameraDeployment-v2')
    try {$cameraLock=$cameraMutex.WaitOne(0)} catch [Threading.AbandonedMutexException] {$cameraLock=$true}
    if(-not $cameraLock){throw 'Another S7 camera deployment is running'}
    Assert-S7CameraBackend 'modern'
    # Require unchanged registrations after acquiring the common deployment lock.
    $current=@(Get-S7CameraRegistration $S7ModernClasses)
    if($oldDll){
     if(@($current|Where-Object {$_.Dll -ne $oldDll -or $_.Key -notlike 'HKLM:\*'}).Count -or $current.Count -ne $(if($previousFrontDll){2}else{1})){throw 'Modern registration changed after preflight'}
     $sourceUnchanged=(Get-FileHash -LiteralPath $oldDll -Algorithm SHA256).Hash -eq $inputHashes['S7Camera.dll']
    } elseif($current.Count){throw 'Camera registration appeared during preflight'}
    $receipt.SourceBinaryUnchanged=$sourceUnchanged
    New-Item -ItemType Directory -Path $LogDirectory | Out-Null
    if($previousEndpointService){
        $currentService=Get-CimInstance Win32_Service -Filter "Name='S7EndpointSync'"
        if(-not $currentService -or $currentService.PathName -ne $previousEndpointService.PathName -or $currentService.StartName -ne 'LocalSystem'){throw 'Endpoint service ownership changed after preflight'}
        $endpointService=Get-Service S7EndpointSync
        if($endpointService.Status -ne 'Stopped'){
            $endpointServiceStopped=$true
            Stop-Service S7EndpointSync -ErrorAction Stop
            $endpointService.WaitForStatus('Stopped',[TimeSpan]::FromSeconds(20))
        }
    }
    $version=$inputHashes['S7Camera.dll'].Substring(0,16)+'-'+[Guid]::NewGuid().ToString('N')
    $target=Join-Path $env:ProgramFiles ('S7 Appliance\Camera\'+$version)
    if(Test-Path -LiteralPath $target){throw 'Version directory exists; refusing to overwrite'}
    New-Item -ItemType Directory -Path $target | Out-Null
    $receipt.Directory=$target
    $receipt.Stage='copy and verify pre-signed files'
    foreach($name in @('S7Camera.dll','S7CameraManage.exe')){
        $file=Join-Path $target $name
        Copy-Item -LiteralPath (Join-Path $BuildDirectory $name) -Destination $file
        if((Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash -ne $inputHashes[$name]){throw "Changed during deployment: $name"}
        Assert-S7PEArchitecture -Path $file -Architecture $nativeArch
        Assert-S7Publisher -Path $file -Thumbprint $PublisherThumbprint
    }
    if($DeliveryDirectory){
        foreach($name in $deliveryFiles){
            $dest=Join-Path "$target/delivery" $name
            $parent=Split-Path $dest -Parent
            if(-not(Test-Path -LiteralPath $parent)){New-Item -ItemType Directory -Path $parent|Out-Null}
            Copy-Item -LiteralPath (Join-Path $DeliveryDirectory $name) -Destination $dest
            if((Get-FileHash -LiteralPath $dest).Hash -ne $deliveryHashes[$name]){throw 'Delivery helper changed during copy'}
        }
        foreach($name in @('bundle.json','bundle.p7s')){Copy-Item -LiteralPath (Join-Path $deliveryRoot $name) -Destination (Join-Path "$target/delivery" $name)}
    }
    if($oldDll -and -not $sourceUnchanged){
        if($RestartCameraService){
            $receipt.Stage='stop approved camera service'
            foreach($name in @('FrameServerMonitor','FrameServer')){
                $service=Get-Service -Name $name -ErrorAction Stop
                if($service.Status -ne 'Stopped'){
                    $restartServices+= $name
                    Stop-Service -Name $name -ErrorAction Stop
                    $service.WaitForStatus('Stopped',[TimeSpan]::FromSeconds(15))
                }
            }
            $receipt.CameraServicesStopped=$restartServices
        }
        $receipt.Stage='release loaded previous source'
        Release-LoadedSource 'runtime-before-upgrade.json'
    }
    $receipt.CaptureTestExecuted=$false
    if($CaptureTest){
        $receipt.Stage='signed live test'
        $ErrorActionPreference='Continue'
        $testOutput=& (Join-Path $target 'S7CameraManage.exe') test (Join-Path $target 'S7Camera.dll') 2>&1
        $testCode=$LASTEXITCODE
        $ErrorActionPreference='Stop'
        $receipt.LiveTestOutput=$testOutput|Out-String
        $testOutput|ForEach-Object{Write-Output $_}
        if($testCode){throw "Signed source failed live camera test: $($receipt.LiveTestOutput)"}
        $receipt.CaptureTestExecuted=$true
    }
    # Configure the stopped service before mutating COM/virtual endpoints.
    # CIM preserves the quoted binary path in built-in Windows PowerShell 5.1.
    $receipt.Stage='install endpoint synchronization service'
    $serviceMode=if($DevelopmentHostPackage){'service-dev'}else{'service'}
    $serviceCommand='"'+(Join-Path $target 'S7CameraManage.exe')+'" '+$serviceMode
    $receipt.DevelopmentHostPackage=[bool]$DevelopmentHostPackage
    if($previousEndpointService){
        $endpointServiceChanged=$true
        Set-S7EndpointServicePath $serviceCommand
    }else{
        New-Service -Name S7EndpointSync -BinaryPathName $serviceCommand -StartupType Automatic -DisplayName 'S7 device states' | Out-Null
        $endpointServiceChanged=$true
    }
    $receipt.Stage='register COM source'
    $mutationStarted=$true
    $registered=$true
    foreach($endpointClass in @($class,$frontClass)){
        $key=New-Item -Path "$endpointClass\InprocServer32" -Force
        Set-Item -LiteralPath $key.PSPath -Value (Join-Path $target 'S7Camera.dll')
        New-ItemProperty -LiteralPath $key.PSPath -Name ThreadingModel -Value Both -PropertyType String -Force|Out-Null
    }
    if($oldDll -and -not $sourceUnchanged){
        # Creation/removal can activate the source inside FrameServerMonitor.
        # Use the newly registered source so a broken old activation cannot
        # prevent its own upgrade. The old registration remains in the receipt.
        $receipt.Stage='remove only previous S7 virtual endpoints'
        & (Join-Path $target 'S7CameraManage.exe') remove
        if($LASTEXITCODE){throw 'Previous S7 virtual endpoint removal failed'}
    }
    $receipt.Stage='verify camera USB transport binding'
    $bindingText=& (Join-Path $target 'S7CameraManage.exe') transport-bind
    if($LASTEXITCODE){throw 'Camera USB driver binding failed'}
    $binding=$bindingText|ConvertFrom-Json
    $receipt.TransportBinding=$binding
    if($binding.reboot_required){throw 'Camera driver requires a Windows restart; no restart was performed'}
    if($binding.present -and (-not $binding.private -or -not $binding.matches)){throw 'Connected phone does not provide the paired WinUSB camera transport'}
    $receipt.Stage='start endpoint synchronization service'
    Start-Service S7EndpointSync -ErrorAction Stop
    (Get-Service S7EndpointSync).WaitForStatus('Running',[TimeSpan]::FromSeconds(20))
    $receipt.EndpointService='Running; applies Enabled from S7, including Camera Off'
    $receipt.Stage='verify registered runtime'
    $runtime=Join-Path $LogDirectory 'runtime-after-install.json'
    & "$PSScriptRoot/Inspect-Runtime.ps1" -OutputFile $runtime -PublisherThumbprint $PublisherThumbprint
    $inspection=Get-Content -LiteralPath $runtime -Raw|ConvertFrom-Json
    if(-not $inspection.Success -or $inspection.Stale){throw "Camera runtime does not match registration: $($inspection.Error)"}
    $receipt.Success=$true
    $receipt.Stage='COM pair and endpoint service installed; runtime acceptance pending'
} catch {
    $receipt.Error=$_.Exception.Message
    if($endpointServiceChanged){
        try{
            Stop-Service S7EndpointSync -ErrorAction Stop
            (Get-Service S7EndpointSync).WaitForStatus('Stopped',[TimeSpan]::FromSeconds(20))
            if($previousEndpointService){
                Set-S7EndpointServicePath $previousEndpointService.PathName
            }else{
                $service=Get-CimInstance Win32_Service -Filter "Name='S7EndpointSync'"
                if($service){$deleted=Invoke-CimMethod -InputObject $service -MethodName Delete;if($deleted.ReturnValue -ne 0){throw "Endpoint service delete failed: $($deleted.ReturnValue)"}}
            }
        }catch{$receipt.EndpointServiceRollbackError=$_.Exception.Message}
    }
    if($mutationStarted){
        try{
            if($registered -and -not $sourceUnchanged){
                & (Join-Path $target 'S7CameraManage.exe') remove
                if($LASTEXITCODE){throw 'Rollback endpoint removal failed; COM ownership preserved'}
            }
            if($oldDll){
                if($registered -and -not $sourceUnchanged){Release-LoadedSource 'runtime-before-rollback.json'}
                New-Item -Path "$class\InprocServer32" -Force|Out-Null
                Set-Item -LiteralPath "$class\InprocServer32" -Value $oldDll
                New-ItemProperty -LiteralPath "$class\InprocServer32" -Name ThreadingModel -Value Both -PropertyType String -Force|Out-Null
                if($previousFrontDll){
                    New-Item -Path "$frontClass\InprocServer32" -Force|Out-Null
                    Set-Item -LiteralPath "$frontClass\InprocServer32" -Value $previousFrontDll
                    New-ItemProperty -LiteralPath "$frontClass\InprocServer32" -Name ThreadingModel -Value Both -PropertyType String -Force|Out-Null
                }elseif(Test-Path -LiteralPath $frontClass){Remove-Item -LiteralPath $frontClass -Recurse}
                if($sourceUnchanged){$receipt.RollbackSucceeded=$true}else{
                    & $oldManager install
                    $receipt.RollbackSucceeded=$LASTEXITCODE -eq 0
                }
            }else{foreach($endpointClass in @($class,$frontClass)){if(Test-Path -LiteralPath $endpointClass){Remove-Item -LiteralPath $endpointClass -Recurse}}}
        }catch{$receipt.RollbackSucceeded=$false;$receipt.RollbackError=$_.Exception.Message}
    }
    Write-Output $receipt.Error
} finally {
    if($endpointServiceStopped -and -not $receipt.Success -and -not $receipt.EndpointServiceRollbackError){
        try{Start-Service S7EndpointSync -ErrorAction Stop}catch{$receipt.EndpointServiceRestoreError=$_.Exception.Message}
    }
    if($restartServices.Count){
        try{
            [array]::Reverse($restartServices)
            foreach($name in $restartServices){Start-Service -Name $name -ErrorAction Stop}
            $receipt.CameraServicesRestored=$true
        }
        catch{$receipt.Success=$false;$receipt.CameraServicesRestored=$false;$receipt.ServiceError=$_.Exception.Message}
    }
    if($cameraLock){$cameraMutex.ReleaseMutex()};if($cameraMutex){$cameraMutex.Dispose()}
    if($writeReceipt){
        if(-not(Test-Path -LiteralPath $LogDirectory)){New-Item -ItemType Directory -Path $LogDirectory | Out-Null}
        $receipt|ConvertTo-Json -Depth 4|Set-Content -LiteralPath $result -Encoding UTF8
        Write-Output "Receipt: $result"
    }
}
if(-not $receipt.Success){throw $receipt.Error}
