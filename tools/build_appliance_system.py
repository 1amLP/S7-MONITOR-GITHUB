#!/usr/bin/env python3
"""Build a deterministic read-only ext4 appliance SYSTEM image.

The tool opens only regular host files. It never mounts, flashes, formats a
device, or reads USERDATA/EFS. Output must be a new directory.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import struct
import uuid

SCHEMA = 'S7-APPLIANCE-SYSTEM-1'
SERIAL_RE = re.compile(r'[0-9a-f]{10,64}')
IMAGE_BYTES = 256 * 1024 * 1024
FIXED_TIME = 1718924016
FIXED_UUID = '53374e41-5050-4c49-414e-434500000001'


def digest(path: Path) -> str:
    h = hashlib.sha256()
    with path.open('rb') as source:
        for chunk in iter(lambda: source.read(1 << 20), b''):
            h.update(chunk)
    return h.hexdigest()


def regular(path: Path) -> Path:
    if path.is_symlink() or not path.is_file():
        raise ValueError('input must be a regular file: ' + str(path))
    return path


def load_bundle(runtime_dir: Path) -> tuple[dict, Path]:
    metadata_path = regular(runtime_dir / 'BUNDLE.json')
    payload = regular(runtime_dir / 'runtime.tar.gz')
    metadata = json.loads(metadata_path.read_text(encoding='utf-8'))
    if (metadata.get('schema') != 'S7-MEDIA-BUNDLE-1' or
            metadata.get('bytes') != payload.stat().st_size or
            metadata.get('sha256') != digest(payload)):
        raise ValueError('media runtime bundle does not match its pin')
    return metadata, payload


def build_stage(runtime_dir: Path, stage: Path, serial: str) -> dict:
    if not SERIAL_RE.fullmatch(serial):
        raise ValueError('invalid pinned S7 serial')
    if stage.exists():
        raise FileExistsError(stage)
    metadata, payload = load_bundle(runtime_dir)
    media = stage / 's7-media'
    media.mkdir(parents=True)
    bundle_name = 'runtime-' + metadata['sha256'] + '.tar.gz'
    copied = media / bundle_name
    shutil.copyfile(payload, copied)
    if digest(copied) != metadata['sha256']:
        raise OSError('runtime copy mismatch')
    consent = media / ('consent-' + metadata['sha256'])
    consent.write_text('S7-NATIVE-MEDIA-1\n' + serial + '\n' + metadata['sha256'] + '\n', encoding='ascii')
    files = []
    for path in sorted(p for p in stage.rglob('*') if p.is_file()):
        files.append({'path': path.relative_to(stage).as_posix(),
                      'bytes': path.stat().st_size, 'sha256': digest(path)})
    manifest = {'schema': SCHEMA, 'serial': serial, 'read_only_runtime': True,
                'android_framework': False, 'userdata_mounted': False,
                'efs_mounted': False, 'runtime': metadata, 'files': files}
    manifest_path = stage / 'SYSTEM_MANIFEST.json'
    manifest_path.write_text(json.dumps(manifest, indent=2) + '\n', encoding='utf-8')
    for path in sorted(stage.rglob('*'), reverse=True):
        if path.is_symlink():
            raise ValueError('generated SYSTEM stage contains a symlink')
        os.utime(path, (FIXED_TIME, FIXED_TIME))
    os.utime(stage, (FIXED_TIME, FIXED_TIME))
    return manifest


def run(command: list[str], env: dict | None = None) -> str:
    result = subprocess.run(command, text=True, capture_output=True, timeout=300,
                            env=(os.environ | (env or {})))
    if result.returncode:
        raise RuntimeError(f'{command[0]} failed ({result.returncode}):\n{result.stdout}\n{result.stderr}')
    return result.stdout + result.stderr


def debugfs_entries(image: Path, directory: str) -> dict[str, int]:
    output = run(['debugfs', '-R', 'ls -p ' + directory, str(image)])
    result = {}
    for line in output.splitlines():
        if not line.startswith('/'):
            continue
        fields = line.split('/')
        if len(fields) < 7 or fields[5] in {'.', '..', ''}:
            continue
        result[fields[5]] = int(fields[1])
    return result


def normalize_ext4(image: Path, output: Path, runtime_sha: str) -> None:
    root = debugfs_entries(image, '/')
    media = debugfs_entries(image, '/s7-media')
    expected_root = {'lost+found', 'SYSTEM_MANIFEST.json', 's7-media'}
    expected_media = {'consent-' + runtime_sha, 'runtime-' + runtime_sha + '.tar.gz'}
    if set(root) != expected_root or set(media) != expected_media:
        raise RuntimeError('unexpected ext4 inode inventory')
    modes = {2: '040755', root['lost+found']: '040700', root['SYSTEM_MANIFEST.json']: '0100444',
             root['s7-media']: '040755', media['consent-' + runtime_sha]: '0100444',
             media['runtime-' + runtime_sha + '.tar.gz']: '0100444'}
    stamp = hex(FIXED_TIME)
    commands = []
    for inode, mode in sorted(modes.items()):
        commands.append(f'set_inode_field <{inode}> mode {mode}')
        for field in ('atime','ctime','mtime','crtime'):
            commands.append(f'set_inode_field <{inode}> {field} {stamp}')
    commands += [f'set_super_value hash_seed {FIXED_UUID}',
                 f'set_super_value wtime {stamp}', f'set_super_value lastcheck {stamp}',
                 f'set_super_value mkfs_time {stamp}']
    script = output / 'normalize.debugfs'
    script.write_text('\n'.join(commands) + '\n', encoding='ascii')
    run(['debugfs', '-w', '-f', str(script), str(image)])
    # debugfs updates the primary write time when it closes and does not copy
    # hash_seed to sparse-super backups. Normalize both fields directly after
    # all filesystem-tool writes. metadata_csum is deliberately disabled.
    block_size, blocks_per_group = 4096, 32768
    groups = (image.stat().st_size // block_size + blocks_per_group - 1) // blocks_per_group
    def power_of(value: int, base: int) -> bool:
        while value > 1 and value % base == 0:
            value //= base
        return value == 1
    locations = [1024]
    for group in range(1, groups):
        if group == 1 or power_of(group,3) or power_of(group,5) or power_of(group,7):
            locations.append(group * blocks_per_group * block_size)
    seed = uuid.UUID(FIXED_UUID).bytes
    with image.open('r+b') as target:
        for start in locations:
            target.seek(start + 48)  # s_wtime
            target.write(struct.pack('<I', FIXED_TIME))
            target.seek(start + 64)  # s_lastcheck
            target.write(struct.pack('<I', FIXED_TIME))
            target.seek(start + 236)  # s_hash_seed[4]
            target.write(seed)
        target.flush()
        os.fsync(target.fileno())


def build(runtime_dir: Path, output: Path, serial: str, image_bytes: int = IMAGE_BYTES) -> dict:
    if output.exists() or image_bytes != IMAGE_BYTES:
        raise ValueError('use a new output directory and the pinned 256 MiB image size')
    for tool in ('mke2fs', 'e2fsck', 'tune2fs', 'debugfs'):
        if not shutil.which(tool):
            raise RuntimeError('missing ext4 build tool: ' + tool)
    output.mkdir(parents=True)
    stage = output / 'root'
    manifest = build_stage(runtime_dir, stage, serial)
    image = output / 'SYSTEM_S7_APPLIANCE.img'
    with image.open('xb') as target:
        target.truncate(image_bytes)
    features = '^64bit,^metadata_csum,^flex_bg,^huge_file,^dir_nlink,^extra_isize,^has_journal'
    env = {'E2FSPROGS_FAKE_TIME': str(FIXED_TIME), 'TZ': 'UTC'}
    run(['mke2fs', '-q', '-F', '-t', 'ext4', '-b', '4096', '-L', 'S7_APPLIANCE',
         '-U', FIXED_UUID, '-O', features, '-E', 'lazy_itable_init=0,lazy_journal_init=0',
         '-d', str(stage), str(image)], env)
    normalize_ext4(image, output, manifest['runtime']['sha256'])
    check = run(['e2fsck', '-fn', str(image)], env)
    info = run(['tune2fs', '-l', str(image)], env)
    listing = run(['debugfs', '-R', 'ls -l /s7-media', str(image)], env)
    if 'runtime-' + manifest['runtime']['sha256'] not in listing:
        raise RuntimeError('runtime is absent from the built ext4 image')
    report = {'schema': SCHEMA, 'serial': serial, 'bytes': image.stat().st_size,
              'sha256': digest(image), 'uuid': FIXED_UUID, 'label': 'S7_APPLIANCE',
              'runtime_sha256': manifest['runtime']['sha256'],
              'source_files': manifest['files'], 'e2fsck': 'PASS',
              'mounted': False, 'flashed': False, 'hardware_tested': False}
    (output / 'SYSTEM_BUILD.json').write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
    (output / 'e2fsck.txt').write_text(check, encoding='utf-8')
    (output / 'tune2fs.txt').write_text(info, encoding='utf-8')
    return report


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--runtime-dir', type=Path, required=True)
    parser.add_argument('--output-dir', type=Path, required=True)
    parser.add_argument('--serial', required=True)
    args = parser.parse_args()
    print(json.dumps(build(args.runtime_dir, args.output_dir, args.serial), indent=2))


if __name__ == '__main__':
    main()
