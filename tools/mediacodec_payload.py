"""BOOT payload: API bridge, pinned runtime identity, first-policy SELinux inputs."""
import hashlib,json,stat
from pathlib import Path

def digest(path:Path):
 h=hashlib.sha256()
 with path.open('rb') as f:
  for chunk in iter(lambda:f.read(1<<20),b''):h.update(chunk)
 return h.hexdigest()

def load(root:Path,monitor='mediacodec',camera='mediacodec'):
 if monitor not in ('mfc','mediacodec') or camera not in ('mfc','mediacodec'):raise ValueError('unsupported codec backend')
 m=json.loads((root/'BRIDGE_MANIFEST.json').read_text());p=root/'bin/mediacodec-bridge'
 if m.get('schema')!='S7-MEDIACODEC-BRIDGE-1' or m.get('path')!='bin/mediacodec-bridge':raise ValueError('unknown MediaCodec bridge')
 if p.is_symlink() or not p.is_file() or p.stat().st_size>256*1024:raise ValueError('invalid MediaCodec bridge extent')
 b=p.read_bytes()
 if b[:7]!=b'\x7fELF\x02\x01\x01' or int.from_bytes(b[18:20],'little')!=183 or len(b)!=m['bytes'] or hashlib.sha256(b).hexdigest()!=m['sha256']:raise ValueError('MediaCodec bridge mismatch')
 pin=(root/'runtime/BUNDLE.json').read_bytes()
 metadata=json.loads(pin)
 runtime=root/'runtime/runtime.tar.gz'
 if metadata['sha256']!=digest(runtime) or metadata['bytes']!=runtime.stat().st_size:raise ValueError('runtime bundle does not match pin')
 result = {'etc/s7-codec/runtime.json':(stat.S_IFREG|0o444,pin),'opt/s7-codec' :(stat.S_IFDIR|0o755,b''),'opt/s7-codec/bin':(stat.S_IFDIR|0o755,b''),
         'opt/s7-codec/bin/mediacodec-bridge':(stat.S_IFREG|0o755,b),'etc/s7-codec':(stat.S_IFDIR|0o755,b''),
         'etc/s7-codec/selection.json':(stat.S_IFREG|0o444,(json.dumps(dict(monitor=monitor,camera=camera))+'\n').encode())}

 sec=root/'security/compiled'
 sp=json.loads((sec/'PIN.json').read_text())
 if sp['schema']!='S7-NATIVE-SELINUX-1' or sp['runtime_sha256']!=metadata['sha256'] or not sp['enforcing_required'] or sp['reload_existing_policy'] or sp['new_domains_permissive']:raise ValueError('invalid SELinux BOOT pin')
 result['etc/s7-security']=(stat.S_IFDIR|0o755,b'')
 result['etc/s7-security/PIN.json']=(stat.S_IFREG|0o444,(sec/'PIN.json').read_bytes())
 for name,key in [('sepolicy',''),('labels.json','labels_')]:
  data=(sec/name).read_bytes()
  if len(data)!=sp[key+'bytes'] or hashlib.sha256(data).hexdigest()!=sp[key+'sha256']:raise ValueError('SELinux policy/labels hash mismatch')
  result['etc/s7-security/'+name]=(stat.S_IFREG|0o444,data)
 return result
