#!/usr/bin/env python3
"""Compile the Mali command renderer against the pinned Bionic ABI."""
from pathlib import Path
import hashlib
import json
import subprocess

ROOT = Path(__file__).resolve().parents[1]

def main():
    source = ROOT / 'native/support/gpu'
    temporary = ROOT / 'native/build/gpu-menu'
    output = ROOT / 'hardware/gpu'
    temporary.mkdir(parents=True, exist_ok=True)
    output.mkdir(parents=True, exist_ok=True)
    kernel = (source / 'menu.cl').read_text()
    (temporary / 'menu_kernel.inc').write_text('static const char menu_kernel[] = ' + json.dumps(kernel) + ';\n')
    binary = output / 'menu-worker-arm64'
    libs = ROOT / 'hardware/sensorhub/vendor/lib64'
    command = ['aarch64-linux-gnu-gcc', '-std=c11', '-nostdlib',
               '-ffreestanding', '-fPIE', '-pie', '-O2', '-fno-stack-protector', '-mno-outline-atomics',
               '-Wall', '-Wextra', '-Werror', '-Wl,--no-undefined,--allow-shlib-undefined,--build-id=none,-z,relro,-z,now,-e,_start',
               '-Wl,--dynamic-linker,/run/s7-gpu-system/system/bin/bootstrap/linker64',
               '-I' + str(temporary), str(source / 'menu_entry.S'), str(source / 'menu_worker.c'),
               '-L' + str(libs), '-l:libc.so', '-l:libdl.so', '-o', str(binary)]
    subprocess.run(command, check=True)
    blob = binary.read_bytes()
    if blob[:7] != b'\x7fELF\x02\x01\x01' or int.from_bytes(blob[18:20], 'little') != 183:
        raise ValueError('menu worker is not ARM64 ELF')
    manifest = dict(schema='S7-GPU-MENU-1', path=binary.name, bytes=len(blob),
                    sha256=hashlib.sha256(blob).hexdigest(),
                    source_sha256=hashlib.sha256((source/'menu_worker.c').read_bytes()).hexdigest(),
                    kernel_sha256=hashlib.sha256((source/'menu.cl').read_bytes()).hexdigest(),
                    cpu_pixel_raster=False, gpu='Mali OpenCL', output='ION DMA-BUF', hardware_tested=False)
    (output/'GPU_MENU_MANIFEST.json').write_text(json.dumps(manifest, indent=2)+'\n')
    print(json.dumps(manifest))

if __name__ == '__main__':
    main()
