[CmdletBinding()]
param(
    [ValidateSet('x64','ARM64')][string]$Platform='x64',
    [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{18}$')][string]$DeviceSerial,
    [string]$VcRoot='C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools\VC\Tools\MSVC\14.44.35207',
    [string]$SdkRoot='C:\Program Files (x86)\Windows Kits\10',
    [string]$KitVersion='10.0.26100.0',
    [string]$OutputDirectory,
    [switch]$SkipTests
)
$ErrorActionPreference='Stop'
$root=Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$vc=$VcRoot
$targetArch=$Platform.ToLowerInvariant()
$sdk=$SdkRoot
$version=$KitVersion
$out=if($OutputDirectory){[IO.Path]::GetFullPath($OutputDirectory)}else{Join-Path $root ('dist/windows-camera/'+$targetArch+'-'+[DateTime]::UtcNow.ToString('yyyyMMdd-HHmmssfff'))}
if(Test-Path -LiteralPath $out){throw 'OutputDirectory already exists; stale artifacts refused'}
New-Item -ItemType Directory -Path $out|Out-Null
$compiler="$vc/bin/Hostx64/$targetArch/cl.exe"
if(-not(Test-Path -LiteralPath $compiler -PathType Leaf)){throw "Missing compiler: $compiler"}
$savedPath=$env:PATH
try {
$env:PATH=(Split-Path $compiler -Parent)+';'+$savedPath
$includes=@("$vc/include","$sdk/Include/$version/ucrt","$sdk/Include/$version/shared","$sdk/Include/$version/um","$sdk/Include/$version/winrt")
$common=@('/nologo','/std:c++20','/EHsc','/W4','/WX','/O2','/MT','/guard:cf','/DUNICODE','/D_UNICODE','/D_WIN32_WINNT=0x0A00','/DNTDDI_VERSION=0x0A00000B',"/DS7_TARGET_SERIAL=L`"$DeviceSerial`"")+@($includes|ForEach-Object{"/I$_"})
$link=@('/link','/DYNAMICBASE','/NXCOMPAT','/GUARD:CF',"/LIBPATH:$vc/lib/$targetArch","/LIBPATH:$sdk/Lib/$version/um/$targetArch","/LIBPATH:$sdk/Lib/$version/ucrt/$targetArch",'mf.lib','mfplat.lib','mfuuid.lib','mfreadwrite.lib','mfsensorgroup.lib','windowscodecs.lib','ole32.lib','uuid.lib','d3d11.lib','hid.lib','setupapi.lib','winusb.lib','newdev.lib')
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common /c "$PSScriptRoot/DeviceIdentity.cpp" "/Fo$out/DeviceIdentity.obj"
if($LASTEXITCODE){throw 'Device identity helper build failed'}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common /LD "$PSScriptRoot/Source.cpp" "$out/DeviceIdentity.obj" "/Fo$out/Source.obj" "/Fe$out/S7Camera.dll" @link advapi32.lib "/DEF:$PSScriptRoot/S7Camera.def"
if($LASTEXITCODE){throw 'Camera source build failed'}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common /c "$PSScriptRoot/EndpointService.cpp" "/Fo$out/EndpointService.obj"
if($LASTEXITCODE){throw 'Endpoint service build failed'}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common /c "$PSScriptRoot/VirtualRegistration.cpp" "/Fo$out/VirtualRegistration.obj"
if($LASTEXITCODE){throw 'Virtual registration helper build failed'}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common /c "$PSScriptRoot/Sniper.cpp" "/Fo$out/Sniper.obj"
if($LASTEXITCODE){throw 'Sniper session helper build failed'}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common /c "$PSScriptRoot/../monitor/DesktopReadback.cpp" "/Fo$out/DesktopReadback.obj"
if($LASTEXITCODE){throw 'Sniper GPU conversion build failed'}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common "$PSScriptRoot/Manage.cpp" "$out/DeviceIdentity.obj" "$out/EndpointService.obj" "$out/VirtualRegistration.obj" "$out/Sniper.obj" "$out/DesktopReadback.obj" "/Fo$out/Manage.obj" "/Fe$out/S7CameraManage.exe" @link cfgmgr32.lib advapi32.lib propsys.lib wtsapi32.lib user32.lib dxgi.lib
if($LASTEXITCODE){throw 'Camera manager build failed'}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common "$PSScriptRoot/Bootstrap.cpp" "$out/DeviceIdentity.obj" "$out/EndpointService.obj" "$out/VirtualRegistration.obj" "$out/Sniper.obj" "$out/DesktopReadback.obj" "/Fo$out/Bootstrap.obj" "/Fe$out/S7PackageBootstrap.exe" @link cfgmgr32.lib advapi32.lib propsys.lib wtsapi32.lib user32.lib dxgi.lib
if($LASTEXITCODE){throw 'PnP bootstrap build failed'}
if(-not $SkipTests){
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common "$PSScriptRoot/tests/endpoint_policy_test.cpp" "/Fo$out/endpoint-policy-test.obj" "/Fe$out/endpoint-policy-test.exe" @link
if($LASTEXITCODE){throw 'Endpoint policy compile-time assertions failed'}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common "$PSScriptRoot/tests/sniper_geometry_test.cpp" "/Fo$out/sniper-geometry-test.obj" "/Fe$out/sniper-geometry-test.exe" @link
if($LASTEXITCODE){throw 'Sniper geometry compile-time assertions failed'}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common "$PSScriptRoot/tests/sniper_mapping_test.cpp" "/Fo$out/sniper-mapping-test.obj" "/Fe$out/sniper-mapping-test.exe" @link
if($LASTEXITCODE){throw 'Sniper pointer identity test build failed'}
if($Platform -eq 'x64' -and -not $SkipTests){
    & "$out/sniper-geometry-test.exe"
    if($LASTEXITCODE){throw 'Sniper transform and control tests failed'}
    & "$out/sniper-mapping-test.exe"
    if($LASTEXITCODE){throw 'Sniper pointer identity tests failed'}
}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common "$PSScriptRoot/nv12-test.cpp" "/Fo$out/nv12-test.obj" "/Fe$out/nv12-test.exe" @link
if($LASTEXITCODE){throw 'NV12 test build failed'}
if($Platform -eq 'x64' -and -not $SkipTests){
    & "$out/nv12-test.exe"
    if($LASTEXITCODE){throw 'NV12 tests failed'}
}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common "$PSScriptRoot/tests/mode_policy_test.cpp" "/Fo$out/mode-policy-test.obj" "/Fe$out/mode-policy-test.exe" @link
if($LASTEXITCODE){throw 'Camera mode policy test build failed'}
if($Platform -eq 'x64' -and -not $SkipTests){
    & "$out/mode-policy-test.exe"
    if($LASTEXITCODE){throw 'Camera mode policy tests failed'}
}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common "$PSScriptRoot/tests/decoded_type_test.cpp" "/Fo$out/decoded-type-test.obj" "/Fe$out/decoded-type-test.exe" @link
if($LASTEXITCODE){throw 'Camera decoded type test build failed'}
if($Platform -eq 'x64' -and -not $SkipTests){
    & "$out/decoded-type-test.exe"
    if($LASTEXITCODE){throw 'Camera decoded type tests failed'}
}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common "$PSScriptRoot/tests/mjpeg_test.cpp" "/Fo$out/mjpeg-test.obj" "/Fe$out/mjpeg-test.exe" @link
if($LASTEXITCODE){throw 'MJPEG test build failed'}
if($Platform -eq 'x64' -and -not $SkipTests){
    & "$out/mjpeg-test.exe"
    if($LASTEXITCODE){throw 'MJPEG tests failed'}
}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common "$PSScriptRoot/tests/selection_protocol_test.cpp" "/Fo$out/selection-test.obj" "/Fe$out/selection-test.exe" @link
if($LASTEXITCODE){throw 'Camera selection ABI test build failed'}
if($Platform -eq 'x64' -and -not $SkipTests){
    & "$out/selection-test.exe"
    if($LASTEXITCODE){throw 'Camera selection ABI tests failed'}
}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common "$PSScriptRoot/tests/lease_retry_test.cpp" "/Fo$out/lease-retry-test.obj" "/Fe$out/lease-retry-test.exe" @link
if($LASTEXITCODE){throw 'Camera lease retry test build failed'}
if($Platform -eq 'x64' -and -not $SkipTests){
    & "$out/lease-retry-test.exe"
    if($LASTEXITCODE){throw 'Camera lease retry tests failed'}
}
& "$vc/bin/Hostx64/$targetArch/cl.exe" @common "$PSScriptRoot/tests/capture_contract_test.cpp" "/Fo$out/capture-contract-test.obj" "/Fe$out/capture-contract-test.exe" @link
if($LASTEXITCODE){throw 'Camera capture contract build failed'}
if($Platform -eq 'x64' -and -not $SkipTests){
    & "$out/capture-contract-test.exe"
    if($LASTEXITCODE){throw 'Camera capture contract tests failed'}
}
}
Get-FileHash "$out/S7Camera.dll","$out/S7CameraManage.exe"|Select-Object Path,Hash|ConvertTo-Json|Set-Content "$out/build.json"
[ordered]@{Architecture=$targetArch;WindowsBuildExecuted=$true;TargetTestsExecuted=($Platform -eq 'x64' -and -not $SkipTests);Signed=$false}|ConvertTo-Json|Set-Content -LiteralPath "$out/build-status.json" -Encoding UTF8
Write-Output $out

} finally {$env:PATH=$savedPath}
