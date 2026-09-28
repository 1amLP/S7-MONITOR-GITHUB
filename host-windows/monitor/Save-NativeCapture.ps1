function Save-NativeCapture {
    param([byte[]]$Packet, [string]$Path)
    if (Test-Path -LiteralPath $Path) { throw "Output exists: $Path" }
    if ($Packet.Length -lt 32 -or [Text.Encoding]::ASCII.GetString($Packet,0,4) -ne 'UI01') { throw 'Invalid capture header' }
    $metaSize = [BitConverter]::ToUInt32($Packet, 12)
    $pixelSize = [BitConverter]::ToUInt32($Packet, 16)
    if ($metaSize -gt 65536 -or $pixelSize -gt 26214400 -or $Packet.Length -ne 32L+$metaSize+$pixelSize) { throw 'Invalid capture extent' }
    $meta = [Text.Encoding]::UTF8.GetString($Packet, 32, $metaSize) | ConvertFrom-Json
    $c = $meta.capture
    if ($c.Width -lt 1 -or $c.Height -lt 1 -or $c.Width -gt 2560 -or $c.Height -gt 2560 -or $c.Rotation -notin @(0,90,180,270)) { throw 'Invalid capture geometry' }
    # This pinned S7 scanout uses B,G,R,X bytes. Never silently swap unfamiliar channels.
    if ($c.Bits -ne 32 -or $c.Red.Offset -ne 16 -or $c.Green.Offset -ne 8 -or $c.Blue.Offset -ne 0 -or
        $c.Red.Length -ne 8 -or $c.Green.Length -ne 8 -or $c.Blue.Length -ne 8 -or
        $c.Red.MSBRight -ne 0 -or $c.Green.MSBRight -ne 0 -or $c.Blue.MSBRight -ne 0 -or
        $c.Stride -ne $c.Width*4 -or $pixelSize -ne $c.Stride*$c.Height) { throw 'Unsupported framebuffer channel layout' }
    Add-Type -AssemblyName System.Drawing
    $bitmap = [Drawing.Bitmap]::new([int]$c.Width,[int]$c.Height,[Drawing.Imaging.PixelFormat]::Format32bppRgb)
    try {
        $locked = $bitmap.LockBits([Drawing.Rectangle]::new(0,0,$c.Width,$c.Height), [Drawing.Imaging.ImageLockMode]::WriteOnly, [Drawing.Imaging.PixelFormat]::Format32bppRgb)
        try {
            if ($locked.Stride -ne $c.Stride) { throw 'Unexpected destination stride' }
            [Runtime.InteropServices.Marshal]::Copy($Packet, [int](32+$metaSize), $locked.Scan0, [int]$pixelSize)
        } finally { $bitmap.UnlockBits($locked) }
        $rotation = @{0=0;90=3;180=2;270=1}[[int]$c.Rotation]
        $bitmap.RotateFlip([Drawing.RotateFlipType]$rotation)
        $file = [IO.File]::Open([IO.Path]::GetFullPath($Path),[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
        try { $bitmap.Save($file,[Drawing.Imaging.ImageFormat]::Png) } finally { $file.Dispose() }
    } finally { $bitmap.Dispose() }
}
