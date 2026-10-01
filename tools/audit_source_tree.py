#!/usr/bin/env python3
"""Offline source-publication checks. This is not firmware acceptance."""
from __future__ import annotations

import argparse
import json
from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import unquote, urlsplit


ROOT = Path(__file__).resolve().parents[1]
TEXT = {'.md', '.go', '.c', '.h', '.cpp', '.hpp', '.cs', '.ps1', '.psm1',
        '.py', '.sh', '.cmd', '.json', '.txt', '.cl', '.inf', '.patch'}
BINARY = {'.exe', '.dll', '.sys', '.cat', '.obj', '.o', '.a', '.lib', '.so',
          '.img', '.bin', '.zip', '.pfx', '.p12', '.key', '.pem', '.cer',
          '.crt', '.p7s', '.log', '.etl', '.dmp', '.mp4', '.png', '.jpg',
          '.gz', '.xz', '.tar', '.7z', '.vhd', '.vhdx', '.pdb', '.cpio',
          '.test', '.sqlite', '.db', '.h264'}
PRIVATE = {
    'private-key': re.compile(r'-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----'),
    'github-token': re.compile(r'\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{20,})\b'),
    'api-key': re.compile(r'\bsk-[A-Za-z0-9_-]{32,}\b'),
    'local-user-path': re.compile(r'(?i)[a-z]:[\\/]+Users[\\/]+(?!Public\b|Default\b)[^\\/\s<>]+'),
    'private-device-serial': re.compile(r'(?<![A-Za-z0-9])[0-9a-fA-F]{18}(?![A-Za-z0-9])'),
}
CYRILLIC = re.compile(r'[\u0400-\u04ff]')
LINK = re.compile(r'!?\[[^\]\n]*\]\(([^)\n]+)\)')


def git(*args: str) -> bytes:
    return subprocess.check_output(['git', '-C', str(ROOT), *args])


def private_findings(text: str, name: str) -> list[dict]:
    # Record locations and categories, never the matching secret itself.
    return [{'file': name, 'line': text.count('\n', 0, m.start()) + 1, 'kind': kind}
            for kind, pattern in PRIVATE.items() for m in pattern.finditer(text)]


def inspect() -> tuple[list[dict], int]:
    files = sorted(set(git('ls-files', '-z', '--cached', '--others', '--exclude-standard').decode('utf-8').split('\0')) - {''})
    findings: list[dict] = []
    checked = 0
    for relative in files:
        path = ROOT / relative
        if path.is_symlink():
            checked += 1
            findings.append({'file': relative, 'kind': 'non-regular-source-entry'})
            continue
        if not path.exists():
            continue  # A staged deletion is not part of the candidate tree.
        checked += 1
        if not path.is_file():
            findings.append({'file': relative, 'kind': 'non-regular-source-entry'})
            continue
        local_hardware = relative.startswith(tuple('hardware/' + name + '/' for name in
                                                  ('firmware', 'fonts', 'gpu', 'mediacodec', 'sensorhub')))
        if path.suffix.lower() in BINARY or local_hardware or any(part in {'reports', 'captures', 'screenshots', 'dist', 'out', 'build', 'artifacts', 'local'} for part in path.relative_to(ROOT).parts):
            findings.append({'file': relative, 'kind': 'local-or-generated-artifact'})
        if path.suffix.lower() not in TEXT and path.name not in {'.gitignore', '.gitattributes', 'LICENSE'}:
            continue
        try:
            text = path.read_text(encoding='utf-8-sig')
        except UnicodeDecodeError:
            findings.append({'file': relative, 'kind': 'non-utf8-source'})
            continue
        findings.extend(private_findings(text, relative))
        if CYRILLIC.search(text):
            findings.append({'file': relative, 'kind': 'non-english-cyrillic-text'})
        if re.search(r'\b[A-Z][A-Z0-9_]*_RU[.]md', text):
            findings.append({'file': relative, 'kind': 'obsolete-document-reference'})
        if path.suffix.lower() != '.md':
            continue
        for match in LINK.finditer(text):
            target = match.group(1).strip().strip('<>')
            parsed = urlsplit(target)
            if parsed.scheme or target.startswith('#'):
                continue
            destination = (path.parent / unquote(parsed.path)).resolve()
            if not destination.is_relative_to(ROOT) or not destination.exists():
                findings.append({'file': relative, 'kind': 'broken-local-link', 'target': target})
    return findings, checked


def inspect_history() -> tuple[list[dict], int]:
    records = git('rev-list', '--objects', '--all').decode('utf-8').splitlines()
    identities = [line.split(' ', 1)[0] for line in records]
    names = {line.split(' ', 1)[0]: line.split(' ', 1)[1] for line in records if ' ' in line}
    info = subprocess.check_output(['git', '-C', str(ROOT), 'cat-file', '--batch-check'],
                                   input=('\n'.join(identities) + '\n').encode()).decode().splitlines()
    blobs = [line.split()[0] for line in info if line.split()[1] == 'blob']
    findings: list[dict] = []
    # One bounded request at a time avoids pipe deadlocks on large histories.
    for oid in blobs:
        data = git('cat-file', 'blob', oid)
        if b'\0' in data:
            continue
        try:
            text = data.decode('utf-8-sig')
        except UnicodeDecodeError:
            continue
        for finding in private_findings(text, names.get(oid, '<historical-blob>')):
            finding['object'] = oid
            findings.append(finding)
    return findings, len(blobs)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--history', action='store_true', help='also scan reachable Git blobs for private data; never rewrite history')
    args = parser.parse_args()
    findings, count = inspect()
    history_count = 0
    if args.history:
        history_findings, history_count = inspect_history()
        findings.extend(history_findings)
    print(json.dumps({'schema': 'PERIMODE_SOURCE_AUDIT_1', 'files_checked': count,
                      'history_blobs_checked': history_count, 'findings': findings,
                      'source_checks_passed': not findings, 'production_ready': False,
                      'scope': 'Pattern and link checks only; no runtime, signature, firmware-rights or hardware acceptance.'}, indent=2))
    return 1 if findings else 0


if __name__ == '__main__':
    sys.exit(main())
