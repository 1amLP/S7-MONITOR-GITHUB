#requires -Version 7.2
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$WdkRoot,
    [string]$WdkPackage,
    [string]$TargetWdkRoot,
    [string]$TargetWdkPackage,
    [string]$WdkToolRoot,
    [ValidatePattern('^(2)\.\d+$')][string]$UmdfVersion='2.25',
    [ValidatePattern('^(1)\.\d+$')][string]$IddCxVersion='1.9',
    [ValidateSet('x64','ARM64')][string]$Platform='x64',
    [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{18}$')][string]$DeviceSerial,
    [string]$OutputDirectory,
    [string]$SdkRoot='C:\Program Files (x86)\Windows Kits\10',
    [string]$VcRoot='C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools\VC\Tools\MSVC\14.44.35207',
    [string]$KitVersion='10.0.26100.0',
    [switch]$TestEncoder,
    [switch]$SkipTests
)
$ErrorActionPreference='Stop'
$project=Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$targetArch=$Platform.ToLowerInvariant()
$targetWdk=if($TargetWdkRoot){$TargetWdkRoot}else{$WdkRoot}
$targetPackage=if($TargetWdkPackage){$TargetWdkPackage}else{$WdkPackage}
$toolRoot=if($WdkToolRoot){$WdkToolRoot}else{$WdkRoot}
$umdfMinor=([version]$UmdfVersion).Minor
$iddcxMinor=([version]$IddCxVersion).Minor
if($umdfMinor -lt 25 -or $iddcxMinor -lt 4){throw 'Frameworks older than UMDF2.25/IddCx1.4 are not supported'}
$machine=if($Platform -eq 'ARM64'){'ARM64'}else{'X64'}
$infArch=if($Platform -eq 'ARM64'){'arm64'}else{'amd64'}
$archFlags=if($Platform -eq 'ARM64'){@('/D_ARM64_','/DARM64')}else{@('/D_AMD64_','/DAMD64')}
$out=if($OutputDirectory){[IO.Path]::GetFullPath($OutputDirectory)}else{Join-Path $project ('dist/windows-local/'+$targetArch+'-'+[DateTime]::UtcNow.ToString('yyyyMMdd-HHmmssfff'))}
if(Test-Path -LiteralPath $out){throw 'OutputDirectory must not already exist; stale binaries are refused'}
$package=Join-Path $out 'package'
New-Item -ItemType Directory -Path $package -Force | Out-Null
$cl="$VcRoot/bin/Hostx64/$targetArch/cl.exe"
$link="$VcRoot/bin/Hostx64/$targetArch/link.exe"
$stamp="$toolRoot/bin/$KitVersion/x64/stampinf.exe"
$verify="$toolRoot/tools/$KitVersion/x64/infverif.exe"
$cat="$toolRoot/bin/$KitVersion/x86/Inf2Cat.exe"
foreach($tool in @($cl,$link,$stamp,$verify,$cat)){
    if(-not(Test-Path -LiteralPath $tool -PathType Leaf)){throw "Missing build tool: $tool"}
}
if(-not $SkipTests){& "$PSScriptRoot/tests/source_contract_test.ps1"}
if($SkipTests -and $TestEncoder){throw 'SkipTests and TestEncoder conflict'}
if($WdkPackage -or $targetPackage){
    foreach($archive in @($WdkPackage,$targetPackage)|Where-Object{$_}|Select-Object -Unique){
        & dotnet nuget verify --all $archive
        if($LASTEXITCODE){throw "WDK NuGet signature verification failed: $archive"}
    }
    Add-Type -AssemblyName System.IO.Compression
}
foreach($tool in @($stamp,$verify,$cat)){
    $signature=Get-AuthenticodeSignature -LiteralPath $tool
    if($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch 'O=Microsoft Corporation'){
        if(-not $WdkPackage){throw "WDK executable requires signed-package verification: $tool"}
        $zip=[IO.Compression.ZipFile]::OpenRead($WdkPackage)
        try {
            $entry=$zip.GetEntry('c/'+[IO.Path]::GetRelativePath($toolRoot,$tool).Replace('\','/'))
            if($null -eq $entry){throw 'Tool missing from verified WDK package'}
            $stream=$entry.Open()
            try {$expected=[Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($stream))} finally {$stream.Dispose()}
            if((Get-FileHash -LiteralPath $tool).Hash -ne $expected){throw 'WDK tool differs from verified package'}
        } finally {$zip.Dispose()}
    }
}
function Checked([string]$Program,[string[]]$Arguments){
    $savedPath=$env:PATH
    try {
        $env:PATH=(Split-Path $Program -Parent)+';'+$savedPath
        & $Program @Arguments
        if($LASTEXITCODE){throw "$Program failed: $LASTEXITCODE"}
    } finally {$env:PATH=$savedPath}
}
$includes=@("$WdkRoot/Include/wdf/umdf/$UmdfVersion","$WdkRoot/Include/$KitVersion/um/iddcx/$IddCxVersion",
    "$WdkRoot/Include/$KitVersion/um","$VcRoot/include","$SdkRoot/Include/$KitVersion/ucrt",
    "$SdkRoot/Include/$KitVersion/shared","$SdkRoot/Include/$KitVersion/um","$SdkRoot/Include/$KitVersion/winrt")
$compile=@('/nologo','/std:c++17','/EHsc','/W4','/WX','/external:W0','/O2','/MD','/guard:cf','/DUNICODE','/D_UNICODE',
    '/D_WIN64','/D_WIN32_WINNT=0x0A00','/DNTDDI_VERSION=0x0A000008',"/DS7_TARGET_SERIAL=L`"$DeviceSerial`"")+
    @($includes|ForEach-Object{"/external:I$_"})+$archFlags
$driverFlags=@('/DUMDF_DRIVER','/DUMDF_VERSION_MAJOR=2',"/DUMDF_VERSION_MINOR=$umdfMinor",'/DUMDF_MINIMUM_VERSION_REQUIRED=25',
    '/DIDDCX_VERSION_MAJOR=1',"/DIDDCX_VERSION_MINOR=$iddcxMinor",'/DIDDCX_MINIMUM_VERSION_REQUIRED=4')
$wdfLibrary=Join-Path $targetWdk "Lib/wdf/umdf/$targetArch/$UmdfVersion/WdfDriverStubUm.lib"
$iddcxLibrary=Join-Path $targetWdk "Lib/$KitVersion/um/$targetArch/iddcx/$IddCxVersion/iddcxstub.lib"
$defaultWdfLibPath="/LIBPATH:$WdkRoot/Lib/wdf/umdf/$targetArch/$UmdfVersion"
$wdfLibPath=if($TargetWdkRoot){"/LIBPATH:$(Split-Path $wdfLibrary -Parent)"}else{$defaultWdfLibPath}
$iddcxLibPath=if($TargetWdkRoot){"/LIBPATH:$(Split-Path $iddcxLibrary -Parent)"}else{"/LIBPATH:$WdkRoot/Lib/$KitVersion/um/$targetArch/iddcx/$IddCxVersion"}
foreach($library in @($wdfLibrary,$iddcxLibrary)){
    if(-not(Test-Path -LiteralPath $library -PathType Leaf)){throw "Missing target WDK library: $library"}
}
if($targetPackage){
    $zip=[IO.Compression.ZipFile]::OpenRead($targetPackage)
    try {
        foreach($library in @($wdfLibrary,$iddcxLibrary)){
            $relative=[IO.Path]::GetRelativePath($targetWdk,$library).Replace('\','/')
            $entry=$zip.Entries|Where-Object{$_.FullName -ieq ('c/'+$relative)}|Select-Object -First 1
            if($null -eq $entry){throw "Target library missing from verified WDK package: $relative"}
            $stream=$entry.Open()
            try {$expected=[Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($stream))} finally {$stream.Dispose()}
            if((Get-FileHash -LiteralPath $library).Hash -ne $expected){throw "Target WDK library differs from verified package: $relative"}
        }
    } finally {$zip.Dispose()}
}
$libraries=@("/LIBPATH:$VcRoot/lib/$targetArch","/LIBPATH:$SdkRoot/Lib/$KitVersion/ucrt/$targetArch",
    "/LIBPATH:$SdkRoot/Lib/$KitVersion/um/$targetArch",$wdfLibPath,$iddcxLibPath)
$commonLibs=@('d3d11.lib','dxgi.lib','mfplat.lib','mf.lib','mfuuid.lib','winusb.lib',
    'setupapi.lib','avrt.lib','ole32.lib','oleaut32.lib','uuid.lib','kernel32.lib','user32.lib','advapi32.lib','wtsapi32.lib')
Checked $cl ($compile+$driverFlags+@('/c',"$PSScriptRoot/Driver.cpp","$PSScriptRoot/Encoder.cpp","$PSScriptRoot/Usb.cpp","$PSScriptRoot/DesktopReadback.cpp","/Fo$out/"))
Checked $link (@('/NOLOGO','/DLL',"/MACHINE:$machine",'/DYNAMICBASE','/NXCOMPAT','/GUARD:CF','/INCREMENTAL:NO',
    "/OUT:$package/S7Monitor.dll","/IMPLIB:$out/S7Monitor.lib","$out/Driver.obj","$out/Encoder.obj","$out/Usb.obj","$out/DesktopReadback.obj")+
    $libraries+@('WdfDriverStubUm.lib','iddcxstub.lib','ntdll.lib')+$commonLibs)
$infSource=if($Platform -eq 'ARM64'){"$PSScriptRoot/S7Monitor-arm64.inf"}else{"$PSScriptRoot/S7Monitor.inf"}
Copy-Item -LiteralPath $infSource -Destination "$package/S7Monitor.inf"
Checked $stamp @('-f',"$package/S7Monitor.inf",'-a',$infArch,'-u',($UmdfVersion+'.0'))
Checked $verify @('/w',"$package/S7Monitor.inf")
$catOS=if($Platform -eq 'ARM64'){'10_CO_ARM64,10_NI_ARM64,10_GE_ARM64'}else{'10_CO_X64,10_NI_X64,10_GE_X64'}
Checked $cat @("/driver:$package","/os:$catOS")
$protocolExecuted=$false
if(-not $SkipTests){
Checked $cl ($compile+@("$PSScriptRoot/tests/protocol_test.cpp","/Fo$out/protocol-test.obj","/Fe$out/protocol-test.exe",'/link')+$libraries)
# Build host is x64; cross-target tests are compiled, never claimed as executed.
$protocolExecuted=$Platform -eq 'x64' -and -not $SkipTests
if($protocolExecuted){Checked "$out/protocol-test.exe" @()}
Checked $cl ($compile+@("$PSScriptRoot/tests/handoff_test.cpp","/Fo$out/handoff-test.obj","/Fe$out/handoff-test.exe",'/link')+$libraries)
if($protocolExecuted){Checked "$out/handoff-test.exe" @()}
Checked $cl ($compile+@("$PSScriptRoot/tests/monitor_slot_test.cpp","/Fo$out/monitor-slot-test.obj","/Fe$out/monitor-slot-test.exe",'/link')+$libraries)
if($protocolExecuted){Checked "$out/monitor-slot-test.exe" @()}
Checked $cl ($compile+@("$PSScriptRoot/tests/lifecycle_test.cpp","/Fo$out/lifecycle-test.obj","/Fe$out/lifecycle-test.exe",'/link')+$libraries)
if($protocolExecuted){Checked "$out/lifecycle-test.exe" @()}
Checked $cl ($compile+@("$PSScriptRoot/tests/nv12_compute_test.cpp","/Fo$out/nv12-compute-test.obj","/Fe$out/nv12-compute-test.exe",'/link')+$libraries)
if($protocolExecuted){Checked "$out/nv12-compute-test.exe" @()}
Checked $cl ($compile+@("$PSScriptRoot/tests/monitor_description_test.cpp","/Fo$out/monitor-description-test.obj","/Fe$out/monitor-description-test.exe",'/link')+$libraries)
if($protocolExecuted){Checked "$out/monitor-description-test.exe" @()}
# D3DCompile checks the real HLSL without creating a GPU device. ARM64 is compile-only.
Checked $cl ($compile+@("$PSScriptRoot/tests/shader_compile_test.cpp","/Fo$out/shader-compile-test.obj","/Fe$out/shader-compile-test.exe",'/link')+$libraries+@('kernel32.lib','ole32.lib'))
if($protocolExecuted){Checked "$out/shader-compile-test.exe" @()}
if($TestEncoder -and $Platform -ne 'x64'){throw 'ARM64 encoder execution requires a native ARM64 test session'}
if($TestEncoder){
    Checked $cl ($compile+@("$PSScriptRoot/Encoder.cpp","$PSScriptRoot/tests/encoder_test.cpp","/Fo$out/","/Fe$out/encoder-test.exe",'/link')+$libraries+$commonLibs)
    Checked "$out/encoder-test.exe" @("$out/synthetic.h264")
}
}
$receipt=[ordered]@{
    SharedUsbIdentitySHA256=(Get-FileHash -LiteralPath (Join-Path $PSScriptRoot '../UsbIdentity.h')).Hash.ToLowerInvariant()
    WarningsAsErrors=$true;ProjectWarningLevel=4;ExternalHeaderWarningLevel=0
    SourceFiles=@(Get-ChildItem -LiteralPath $PSScriptRoot -File|Where-Object{$_.Extension -in @('.cpp','.h')}|Sort-Object Name|ForEach-Object{[ordered]@{Name=$_.Name;SHA256=(Get-FileHash -LiteralPath $_.FullName).Hash.ToLowerInvariant()}})
    Architecture=$targetArch;SourceContractTestExecuted=(-not $SkipTests);ProtocolTestExecuted=$protocolExecuted;HandoffTestExecuted=$protocolExecuted;MonitorSlotTestExecuted=$protocolExecuted;LifecycleTestExecuted=$protocolExecuted;NV12ComputeMathTestExecuted=$protocolExecuted;MonitorDescriptionTestExecuted=$protocolExecuted;ShaderCompiled=$protocolExecuted
    WdkRoot=$WdkRoot;TargetWdkRoot=$targetWdk;TargetWdkPackageSHA256=$(if($targetPackage){(Get-FileHash -LiteralPath $targetPackage).Hash.ToLowerInvariant()}else{$null});Kit=$KitVersion;UMDF=$UmdfVersion;IddCx=$IddCxVersion;MinimumUMDF='2.25';MinimumIddCx='1.4'
    DriverCompiledAndLinked=$true;InfVerif=$true;CatalogGenerated=$true
    SyntheticHardwareTest=[bool]$TestEncoder;Signed=$false;Installed=$false
    Runtime='MSVC dynamic CRT; local development build, not production Spectre-library validation'
    Files=@(Get-ChildItem -LiteralPath $package -File|ForEach-Object{
        [ordered]@{Name=$_.Name;SHA256=(Get-FileHash -LiteralPath $_.FullName).Hash.ToLowerInvariant()}
    })
}
$receipt|ConvertTo-Json -Depth 4|Set-Content -LiteralPath "$out/build.json" -Encoding utf8
Write-Output "Unsigned package: $package"
Write-Output 'No certificate stores, driver settings, devices or phone state were changed.'
