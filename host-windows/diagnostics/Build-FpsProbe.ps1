[CmdletBinding()]
param(
 [ValidateSet('x64','ARM64')][string]$Platform='x64',
 [string]$VcRoot='C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools\VC\Tools\MSVC\14.44.35207',
 [string]$SdkRoot='C:\Program Files (x86)\Windows Kits\10',
 [string]$KitVersion='10.0.26100.0',
 [string]$OutputDirectory=(Join-Path $PSScriptRoot 'build-fps')
)
$ErrorActionPreference='Stop'
$arch=$Platform.ToLowerInvariant()
$compiler="$VcRoot/bin/Hostx64/$arch/cl.exe"
if(-not(Test-Path -LiteralPath $compiler -PathType Leaf)){throw "Missing compiler: $compiler"}
if(Test-Path -LiteralPath $OutputDirectory){throw 'OutputDirectory already exists; stale output refused'}
New-Item -ItemType Directory -Path $OutputDirectory | Out-Null
$output=(Resolve-Path -LiteralPath $OutputDirectory).Path
$savedPath=$env:PATH
try {
$env:PATH=(Split-Path $compiler -Parent)+';'+$savedPath
$includes=@("$VcRoot/include","$SdkRoot/Include/$KitVersion/ucrt","$SdkRoot/Include/$KitVersion/shared","$SdkRoot/Include/$KitVersion/um","$SdkRoot/Include/$KitVersion/winrt")
$flags=@('/nologo','/std:c++20','/EHsc','/W4','/O2','/MT','/guard:cf','/DUNICODE','/D_UNICODE')+@($includes|ForEach-Object{"/I$_"})
$libs=@('/link','/DYNAMICBASE','/NXCOMPAT','/GUARD:CF',"/LIBPATH:$VcRoot/lib/$arch","/LIBPATH:$SdkRoot/Lib/$KitVersion/um/$arch","/LIBPATH:$SdkRoot/Lib/$KitVersion/ucrt/$arch",'mf.lib','mfplat.lib','mfreadwrite.lib','mfuuid.lib','ole32.lib')
& $compiler @flags (Join-Path $PSScriptRoot 'camera-fps.cpp') "/Fo$output/camera-fps.obj" "/Fe$output/camera-fps.exe" @libs
if($LASTEXITCODE){throw "Probe build failed: $LASTEXITCODE"}
Write-Output (Join-Path $output 'camera-fps.exe')

} finally {$env:PATH=$savedPath}
