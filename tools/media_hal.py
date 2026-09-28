"""Explicit dlopen roots of the selected original HIDL/OMX runtime.

ELF DT_NEEDED does not describe passthrough HAL loading. Keep an explicit list
for the *selected* services; never import the entire Android HAL directory.
"""
from __future__ import annotations
import hashlib
from pathlib import Path
import re
import subprocess

ALLOCATOR = 'vendor/lib64/hw/android.hardware.graphics.allocator@2.0-impl.so'
LSHAL = 'system/bin/lshal'
EXTRA_ROOTS = (ALLOCATOR, LSHAL)

def elf(path: Path) -> dict:
    with path.open('rb') as f:
        h = f.read(20)
    if len(h) != 20 or h[:4] != b'\x7fELF' or h[4] not in (1, 2) or h[5] != 1:
        raise ValueError('not a little-endian ELF: ' + str(path))
    bits = 32 if h[4] == 1 else 64
    if int.from_bytes(h[18:20], 'little') != (40 if bits == 32 else 183):
        raise ValueError('non-ARM runtime object: ' + str(path))
    dyn = subprocess.run(['readelf', '-Wd', str(path)], check=True, capture_output=True, text=True).stdout
    syms = subprocess.run(['readelf', '-W', '--dyn-syms', str(path)], check=True, capture_output=True, text=True).stdout
    exports = set()
    for line in syms.splitlines():
        s = line.split()
        if len(s) >= 8 and s[4] in ('GLOBAL', 'WEAK') and s[5] in ('DEFAULT', 'PROTECTED') and s[6] != 'UND':
            exports.add(s[7].split('@')[0])
    return {'bits': bits, 'needed': re.findall(r'\(NEEDED\).*?\[(.*?)\]', dyn), 'exports': exports}

def required_roots():
    roots = {ALLOCATOR: (64, 'HIDL_FETCH_IAllocator'), LSHAL: (64, None)}
    for lib, bits in (('lib', 32), ('lib64', 64)):
        roots.update({
            f'vendor/{lib}/hw/gralloc.exynos5.so': (bits, 'HMI'),
            f'vendor/{lib}/hw/android.hardware.graphics.mapper@2.0-impl-2.1.so': (bits, 'HIDL_FETCH_IMapper'),
            f'vendor/{lib}/hw/android.hidl.memory@1.0-impl.so': (bits, 'HIDL_FETCH_IMapper'),
            f'system/{lib}/hw/android.hidl.memory@1.0-impl.so': (bits, 'HIDL_FETCH_IMapper'),
            f'vendor/{lib}/libstagefrighthw.so': (bits, '_ZN7android15createOMXPluginEv'),
            f'vendor/{lib}/omx/libOMX.Exynos.AVC.Decoder.so': (bits, 'Exynos_OMX_ComponentInit'),
            f'vendor/{lib}/omx/libOMX.Exynos.AVC.Encoder.so': (bits, 'Exynos_OMX_ComponentInit'),
        })
    return roots

def validate(stage: Path, records: dict) -> dict:
    roots = []
    for name, (bits, symbol) in required_roots().items():
        p = stage/name
        if name not in records or p.is_symlink() or not p.is_file():
            raise ValueError('missing selected media dlopen root: ' + name)
        info = elf(p)
        if info['bits'] != bits or (symbol and symbol not in info['exports']):
            raise ValueError('wrong media HAL architecture/entry point: ' + name)
        digest = hashlib.sha256(p.read_bytes()).hexdigest()
        if digest != records[name]['sha256']:
            raise ValueError('media HAL record mismatch: ' + name)
        roots.append({'path': name, 'elf_bits': bits, 'entry_point': symbol, 'sha256': digest})
    return {'schema': 'S7-MEDIA-HAL-ROOTS-1', 'roots': roots, 'original_bytes_unmodified': True,
            'runtime_executed': False, 'scope': 'explicit selected dlopen roots, not proof of HIDL registration or frames'}
