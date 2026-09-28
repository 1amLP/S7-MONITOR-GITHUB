[CmdletBinding()]
param([ValidateSet('x64','ARM64')][string]$Platform='x64',
 [string]$VcRoot='C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools\VC\Tools\MSVC\14.44.35207',
 [string]$SdkRoot='C:\Program Files (x86)\Windows Kits\10',[string]$KitVersion='10.0.26100.0',
 [Parameter(Mandatory)][string]$OutputDirectory)
$ErrorActionPreference='Stop'
$out=[IO.Path]::GetFullPath($OutputDirectory)
if(Test-Path -LiteralPath $out){throw 'Use a new build directory'}
New-Item -ItemType Directory -Path $out|Out-Null
$arch=$Platform.ToLowerInvariant()
$compiler=Join-Path $VcRoot "bin/Hostx64/$arch/cl.exe"
$inc=@("$VcRoot/include","$SdkRoot/Include/$KitVersion/ucrt","$SdkRoot/Include/$KitVersion/shared","$SdkRoot/Include/$KitVersion/um")
$compile=@('/nologo','/std:c++20','/EHsc','/W4','/WX','/O2','/MT','/guard:cf','/DUNICODE','/D_UNICODE','/D_WIN32_WINNT=0x0A00')+@($inc|ForEach-Object{"/I$_"})
$link=@('/link','/DYNAMICBASE','/NXCOMPAT','/GUARD:CF',"/LIBPATH:$VcRoot/lib/$arch","/LIBPATH:$SdkRoot/Lib/$KitVersion/um/$arch","/LIBPATH:$SdkRoot/Lib/$KitVersion/ucrt/$arch",'setupapi.lib','newdev.lib','wintrust.lib','crypt32.lib','bcrypt.lib','uuid.lib')
$link += 'shell32.lib'
& $compiler @compile "$PSScriptRoot/Setup.cpp" "/Fo$out/Setup.obj" "/Fe$out/S7Setup.exe" @link
if($LASTEXITCODE){throw 'S7 native setup build failed'}
Get-FileHash -LiteralPath "$out/S7Setup.exe"|Select-Object Path,Hash|ConvertTo-Json|Set-Content -LiteralPath "$out/build.json" -Encoding UTF8
Write-Output 'Unsigned installer helper built. No device or installed driver was changed.'
