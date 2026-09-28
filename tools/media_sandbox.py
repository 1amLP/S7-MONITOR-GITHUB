#!/usr/bin/env python3
"""Resolve original minijail policy dependencies without modifying syscall rules.

This is a file dependency resolver, not a BPF compiler or a replacement sandbox.
The retained minijail parser only supports one include level. Do not accept a
nested graph merely because the bundler can traverse it.
"""
from __future__ import annotations
import re
from pathlib import Path
from typing import Callable, Iterable

MAX_POLICY = 65536
MAX_FILES = 64
MAX_TOTAL = 1 << 20
_POLICY = re.compile(r"/(?:system|vendor|apex/[A-Za-z0-9_.-]+)/etc/seccomp_policy/[A-Za-z0-9_.-]+\.policy\Z")


def policy_path(name: str) -> str:
    if not _POLICY.fullmatch(name) or any(p in ('.', '..') for p in name.split('/')):
        raise ValueError('noncanonical/out-of-scope sandbox policy: ' + name)
    return name


def includes(name: str, data: bytes) -> list[str]:
    policy_path(name)
    if not data or len(data) > MAX_POLICY or b'\0' in data:
        raise ValueError('invalid sandbox policy extent: ' + name)
    refs = []
    for number, line in enumerate(data.decode('ascii').splitlines(), 1):
        line = line.strip()
        if not line or line.startswith('#') or not line.startswith('@'):
            continue
        # Exact original minijail @include syntax, not shell/XML expansion.
        if not line.startswith('@include ') or line[9:] != line[9:].strip():
            raise ValueError(f'unsupported policy directive {name}:{number}')
        refs.append(policy_path(line[9:]))
    return refs


def resolve(roots: Iterable[str], read: Callable[[str], bytes]) -> dict:
    data: dict[str, bytes] = {}
    edges = []
    total = 0

    def visit(name: str, depth: int, ancestors: tuple[str, ...]) -> None:
        nonlocal total
        policy_path(name)
        if name in ancestors:
            raise ValueError('cyclic sandbox policy: ' + name)
        if name not in data:
            if len(data) >= MAX_FILES:
                raise ValueError('too many sandbox policies')
            payload = read(name)
            if len(payload) > MAX_POLICY:
                raise ValueError('oversize sandbox policy: ' + name)
            total += len(payload)
            if total > MAX_TOTAL:
                raise ValueError('sandbox policy tree too large')
            data[name] = payload
        refs = includes(name, data[name])
        if depth and refs:
            raise ValueError('minijail does not admit nested @include: ' + name)
        for target in refs:
            edge = {'from': name, 'to': target}
            if edge not in edges:
                edges.append(edge)
            visit(target, depth + 1, ancestors + (name,))

    for name in sorted(set(roots)):
        visit(name, 0, ())
    return {'files': sorted(data), 'edges': edges, 'total_bytes': total,
            'original_rules_modified': False, 'bpf_compiled': False}


def read_staged(stage: Path, name: str) -> bytes:
    policy_path(name)
    target = stage
    # Every component must be a real directory/file inside a private staging root.
    for component in name.lstrip('/').split('/'):
        target = target / component
        if target.is_symlink():
            raise ValueError('symlink sandbox source: ' + name)
    if not target.is_file():
        raise ValueError('missing sandbox @include: ' + name)
    with target.open('rb') as stream:
        return stream.read(MAX_POLICY + 1)


def roots_from_records(records: dict) -> list[str]:
    return sorted('/' + name for name in records if '/etc/seccomp_policy/' in name)


def complete(stage: Path, records: dict, obtain: Callable[[str], None]) -> dict:
    if 'system/etc/seccomp_policy/mediacodec.policy' not in records:
        raise ValueError('missing mandatory OMX system sandbox policy')
    def read(name: str) -> bytes:
        if name.lstrip('/') not in records:
            obtain(name)
        return read_staged(stage, name)
    return resolve(roots_from_records(records), read)
