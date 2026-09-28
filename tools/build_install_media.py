#!/usr/bin/env python3
"""Build a read-only installer disk as a regular host artifact, never a device."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess

FILES = ('S7Setup.exe', 'Install-FromMedia.ps1', 'PackageCommon.ps1',
         'PhonePackage.ps1', 'PHONE_PACKAGE.json', 'package.zip', 'autorun.inf')


def sha(path):
    h = hashlib.sha256()
    with path.open('rb') as source:
        for part in iter(lambda: source.read(1 << 20), b''):
            h.update(part)
    return h.hexdigest()


def build(source, output):
    if output.exists():
        raise ValueError('use a new output directory')
    paths = [source / name for name in FILES]
    if any(p.is_symlink() or not p.is_file() for p in paths):
        raise ValueError('installer media requires the exact regular-file payload')
    if {p.name for p in source.iterdir()} != set(FILES):
        raise ValueError('unexpected installer media member')
    pin = json.loads((source / 'PHONE_PACKAGE.json').read_text(encoding='utf-8-sig'))
    package = source / 'package.zip'
    if pin['schema'] != 'S7_PHONE_PACKAGE_1' or pin['bytes'] != package.stat().st_size or pin['sha256'] != sha(package):
        raise ValueError('package pin mismatch')
    size = 16 << 20
    needed = sum(p.stat().st_size for p in paths) + (4 << 20)
    while size < needed:
        size *= 2
    if size > 256 << 20:
        raise ValueError('installer disk exceeds limit')
    for tool in ('mkfs.vfat', 'mcopy'):
        if not shutil.which(tool):
            raise RuntimeError('missing build tool: ' + tool)
    output.mkdir(parents=True)
    image = output / 'install.img'
    with image.open('xb') as target:
        target.truncate(size)
    def run(args):
        subprocess.run(args, check=True, capture_output=True, timeout=120)
    run(['mkfs.vfat', '--invariant', '-F', '16', '-n', 'S7SETUP', str(image)])
    verify = output / 'readback'
    verify.mkdir()
    for path in paths:
        run(['mcopy', '-i', str(image), str(path), '::/' + path.name])
        copied = verify / path.name
        run(['mcopy', '-i', str(image), '::/' + path.name, str(copied)])
        if copied.stat().st_size != path.stat().st_size or sha(copied) != sha(path):
            raise RuntimeError('FAT readback mismatch: ' + path.name)
    pin.update(media_bytes=size, media_sha256=sha(image))
    (output / 'PHONE_PACKAGE.json').write_text(json.dumps(pin, sort_keys=True, indent=2) + '\n', encoding='ascii')
    return pin


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    print(json.dumps(build(args.source, args.output), indent=2))
