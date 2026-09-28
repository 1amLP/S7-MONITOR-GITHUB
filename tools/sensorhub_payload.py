"""Bounded, exact allowlist for the private sensor-hub runtime in native BOOT."""
from __future__ import annotations
import hashlib,json,stat
from pathlib import Path,PurePosixPath
NAMES={'bin/lhd','bin/linker64','lhd.conf','lib64/libc.so','lib64/libdl.so','lib64/libm.so','lib64/libc++.so','lib64/liblog.so','lib64/libutils.so','lib64/libhardware_legacy.so'}
LHD_SHA='22075698ef8f3ee761a8a88558bce9483a021fc4c6eb032d79b1c493c1887c49'
def load(root:Path):
 raw=(root/'RUNTIME_MANIFEST.json').read_bytes()
 if len(raw)>32768:raise ValueError('oversize hub manifest')
 m=json.loads(raw)
 if m.get('schema')!='S7_SENSORHUB_RUNTIME_1' or m.get('prefix')!='opt/s7-hub' or m.get('lhd_sha256')!=LHD_SHA or m.get('android_services')!=[]:raise ValueError('unexpected sensorhub contract')
 result={};seen=set();total=0
 for item in m['files']:
  n=item['path'];p=root/'runtime'/n
  if n not in NAMES or n in seen:raise ValueError('extra/duplicate runtime path')
  seen.add(n)
  if p.is_symlink() or not p.is_file() or any(q.is_symlink() for q in p.parents):raise ValueError('unsafe hub payload file')
  b=p.read_bytes();total+=len(b)
  if len(b)>4<<20 or total>12<<20 or len(b)!=item['bytes'] or hashlib.sha256(b).hexdigest()!=item['sha256']:raise ValueError('hub file mismatch: '+n)
  mode=0o755 if n.startswith('bin/') else 0o644
  if item['mode']!=mode:raise ValueError('hub file mode: '+n)
  if n=='bin/lhd' and item['sha256']!=LHD_SHA:raise ValueError('unrecognized lhd')
  if n!='lhd.conf' and (b[:7]!=b'\x7fELF\x02\x01\x01' or int.from_bytes(b[18:20],'little')!=183):raise ValueError('non AArch64 runtime ELF')
  result['opt/s7-hub/'+n]=(stat.S_IFREG|mode,b)
 if seen!=NAMES:raise ValueError('incomplete runtime payload')
 result['etc/s7-sensorhub.json']=(stat.S_IFREG|0o444,raw)
 result['system/bin/linker64']=(stat.S_IFLNK|0o777,b'/opt/s7-hub/bin/linker64')
 # No APK, service rc, framework, camera/sensors HAL or binder implementation.
 return result
