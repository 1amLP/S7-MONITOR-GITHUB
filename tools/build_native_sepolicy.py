#!/usr/bin/env python3
"""Compile the appliance policy/labels using libsepol and libselinux on the build host.

Original CIL/ELF bytes remain intact. Two unused source permissive declarations
are removed from compiler input. Additions may only grant to/from new s7_ types;
no existing Android-to-Android access is added. Android's full-OS neverallow
assertions conflict in the supplied vendor CIL itself and with a non-Android
PID1; they are NOT claimed to pass. Appliance-specific denies are queried in the
compiled binary by test_native_security.py. No policy is loaded on this host.
"""
from __future__ import annotations
import ctypes as C, hashlib, json, os, re, stat, tarfile, tempfile
from pathlib import Path
ROOT=Path(__file__).resolve().parents[1]
INPUTS=('system/etc/selinux/plat_sepolicy.cil','system/etc/selinux/mapping/34.0.cil',
'system/system_ext/etc/selinux/system_ext_sepolicy.cil','system/system_ext/etc/selinux/mapping/34.0.cil',
'system/vendor/etc/selinux/plat_pub_versioned.cil','system/vendor/etc/selinux/vendor_sepolicy.cil')

def sha(b):return hashlib.sha256(b).hexdigest()

def compile_policy(inputs):
 s=C.CDLL('libsepol.so.2');V=C.c_void_p
 s.cil_db_init.argtypes=[C.POINTER(V)];s.cil_db_destroy.argtypes=[C.POINTER(V)]
 s.cil_add_file.argtypes=[V,C.c_char_p,C.c_char_p,C.c_size_t];s.cil_add_file.restype=C.c_int
 s.cil_compile.argtypes=[V];s.cil_compile.restype=C.c_int
 s.cil_build_policydb.argtypes=[V,C.POINTER(V)];s.cil_build_policydb.restype=C.c_int
 s.sepol_policydb_to_image.argtypes=[V,V,C.POINTER(V),C.POINTER(C.c_size_t)];s.sepol_policydb_to_image.restype=C.c_int
 s.sepol_policydb_free.argtypes=[V]
 db=V();policy=V();data=V();size=C.c_size_t();s.cil_db_init(C.byref(db))
 try:
  for name,val in [('cil_set_mls',1),('cil_set_multiple_decls',1),('cil_set_policy_version',30),('cil_set_attrs_expand_generated',1),('cil_set_disable_neverallow',1)]:
   f=getattr(s,name);f.argtypes=[V,C.c_int];f(db,val)
  for n,b in inputs:
   if s.cil_add_file(db,n.encode(),b,len(b)):raise ValueError('CIL parse: '+n)
  if s.cil_compile(db) or s.cil_build_policydb(db,C.byref(policy)):raise ValueError('CIL build failed')
  if s.sepol_policydb_to_image(None,policy,C.byref(data),C.byref(size)):raise ValueError('CIL serialize failed')
  return C.string_at(data,size.value)
 finally:
  if data.value:
   lib=C.CDLL(None);lib.free.argtypes=[V];lib.free(data)
  if policy.value:s.sepol_policydb_free(policy)
  s.cil_db_destroy(C.byref(db))

class Labeler:
 def __init__(self,path):
  self.lib=C.CDLL('libselinux.so.1',use_errno=True);self.lib.selabel_open.argtypes=[C.c_uint,C.c_void_p,C.c_uint];self.lib.selabel_open.restype=C.c_void_p
  self.lib.selabel_lookup_raw.argtypes=[C.c_void_p,C.POINTER(C.c_void_p),C.c_char_p,C.c_int]
  self.lib.freecon.argtypes=[C.c_void_p];self.lib.selabel_close.argtypes=[C.c_void_p]
  class Opt(C.Structure):_fields_=[('type',C.c_int),('value',C.c_char_p)]
  option=Opt(3,os.fsencode(path)) # SELABEL_OPT_PATH; SELABEL_CTX_FILE = 0
  self.handle=self.lib.selabel_open(0,C.byref(option),1)
  if not self.handle:raise OSError(C.get_errno(),'selabel_open')
 def lookup(self,path,mode):
  out=C.c_void_p()
  if self.lib.selabel_lookup_raw(self.handle,C.byref(out),path.encode(),mode):raise ValueError('no stock file context: '+path)
  try:return C.string_at(out).decode()
  finally:self.lib.freecon(out)
 def close(self):self.lib.selabel_close(self.handle)

def validate_overlay(text):
 # No source-domain attribute expansion, old type alias, permissive, broad all,
 # or existing->existing grants. This is a build admission check, not a proof
 # of compatibility with a real kernel/device.
 for line in text.splitlines():
  line=line.strip()
  if not line or line.startswith(';'):continue
  verb=line.split()[0]
  if verb in ('(allow','(allowx','(typetransition'):
   words=line.split();src,dst=words[1:3]
   if not (src.startswith('s7_') or dst.startswith('s7_')):raise ValueError('unscoped policy addition: '+line)
  elif verb=='(type':
   if not line.split()[1].startswith('s7_'):raise ValueError('foreign type')
  elif verb=='(roletype':
   if not line.split()[2].startswith('s7_'):raise ValueError('foreign role member')
  elif verb=='(typeattributeset':
   if not re.fullmatch(r'\(typeattributeset (?:file_type|exec_type|system_file_type) \(s7_\w+\)\)',line):raise ValueError('foreign attribute expansion')
  elif verb=='(neverallow':
   if not line.split()[1].startswith('s7_'):raise ValueError('foreign assertion')
  else:raise ValueError('unapproved policy operation: '+verb)

DYNAMIC={'/':'rootfs','/dev':'device','/dev/socket':'socket_device','/data':'system_data_root_file',
 '/run':'tmpfs','/tmp':'tmpfs','/s7-control':'s7_media_control','/dev/s7-cgroup':'cgroup_v2',
 '/dev/cgroup_info':'cgroup_rc_file','/dev/cgroup_info/cgroup.rc':'cgroup_rc_file',
 '/dev/.s7-media-devices-ready':'device'}
DEVICES={'null':'null_device','zero':'zero_device','random':'random_device','urandom':'random_device',
 'binder':'binder_device','hwbinder':'hwbinder_device','vndbinder':'vndbinder_device','ashmem':'ashmem_device',
 'ion':'ion_device','kmsg':'kmsg_device','mali0':'gpu_device','pmsg0':'pmsg_device'}

def build(root=ROOT):
 sec=root/'hardware/mediacodec/security';source=json.loads((sec/'INPUTS.json').read_text())
 for rec in source['files']:
  data=(sec/rec['path']).read_bytes()
  if len(data)!=rec['bytes'] or sha(data)!=rec['sha256']:raise ValueError('security input pin changed: '+rec['path'])
 inputs=[];removed=[]
 for name in INPUTS:
  b=(sec/name).read_bytes()
  for domain in re.findall(rb'^\(typepermissive (\w+)\)\n',b,re.M):
   if domain not in (b'su',b'backuptool'):raise ValueError('unexpected source permissive')
   removed.append(domain.decode());b=b.replace(b'(typepermissive '+domain+b')\n',b'')
  inputs.append((name,b))
 overlay=(root/'native/support/selinux/s7-native.cil').read_text();validate_overlay(overlay)
 policy=compile_policy(inputs+[('s7-native.cil',overlay.encode())])
 # Runtime still uses unmodified stock file_contexts. Expand them at build time
 # into exact paths: target boot needs neither PCRE nor Android libselinux.
 contexts=b'\n'.join((sec/n).read_bytes() for n in ['system/etc/selinux/plat_file_contexts','system/vendor/etc/selinux/vendor_file_contexts','system/system_ext/etc/selinux/system_ext_file_contexts'])
 labels=[]
 with tempfile.TemporaryDirectory(prefix='s7-contexts-') as temp:
  file=Path(temp)/'file_contexts';file.write_bytes(contexts);lab=Labeler(file)
  try:
   with tarfile.open(root/'hardware/mediacodec/runtime/runtime.tar.gz','r:gz') as tar:
    for m in tar:
     path='/'+m.name.rstrip('/');kind='dir' if m.isdir() else 'file'
     if not m.isdir() and not m.isfile():raise ValueError('runtime symlink/special entry')
     if path.startswith('/apex/'):
      if path.split('/')[2] == 'com.android.media':
       allowed_dirs=('/apex/com.android.media','/apex/com.android.media/etc','/apex/com.android.media/etc/seccomp_policy')
       allowed_files=tuple('/apex/com.android.media/etc/seccomp_policy/'+n for n in ('crash_dump.arm64.policy','code_coverage.arm64.policy'))
       if not (m.isdir() and path in allowed_dirs or m.isfile() and path in allowed_files):raise ValueError('unreviewed media APEX payload')
      elif path.split('/')[2] not in ('com.android.i18n','com.android.os.statsd'):raise ValueError('unreviewed flattened APEX labels')
      ty='system_lib_file' if m.isfile() and '.so' in Path(path).name else 'system_file'
     elif path in ('/mnt/androidwritable','/mnt/installer'):ty='mnt_user_file'
     elif path=='/s7/native':ty='s7_native_exec'
     elif path=='/s7/mediacodec-bridge':ty='s7_codec_exec'
     elif path.startswith('/s7/') or path in ('/s7','/bootstrap-apex'):ty='rootfs'
     elif path in DYNAMIC:ty=DYNAMIC[path]
     else:ty=None
     context='u:object_r:'+ty+':s0' if ty else lab.lookup(path,stat.S_IFDIR if m.isdir() else stat.S_IFREG)
     labels.append({'path':path,'kind':kind,'context':context})
  finally:lab.close()
 for name,ty in DEVICES.items():labels.append({'path':'/dev/'+name,'kind':'char','context':'u:object_r:'+ty+':s0','optional':name in ('kmsg','mali0','pmsg0')})
 for path in ('/opt/s7-gpu','/opt/s7-gpu/bin'):
  labels.append({'path':path,'kind':'dir','context':'u:object_r:rootfs:s0','optional':True})
 labels.append({'path':'/opt/s7-gpu/bin/gpu-probe-arm64','kind':'file','context':'u:object_r:s7_native_exec:s0','optional':True})
 labels.append({'path':'/opt/s7-gpu/bin/menu-worker-arm64','kind':'file','context':'u:object_r:s7_native_exec:s0','optional':True})
 for path,ty in DYNAMIC.items():
  if path=='/' or not any(i['path']==path for i in labels):
   labels.append({'path':path,'kind':'file' if path.endswith(('cgroup.rc','devices-ready')) else 'dir','context':'u:object_r:'+ty+':s0'})
 # /dev/socket is after the private /dev tmpfs mount. Device entries must be
 # applied after mount; immutable bundle entries before readonly remount.
 plan={'schema':'S7-SELINUX-LABELS-1','runtime_sha256':json.loads((root/'hardware/mediacodec/runtime/BUNDLE.json').read_text())['sha256'],
 'files':sorted(labels,key=lambda x:x['path']),'video_context':'u:object_r:video_device:s0'}
 out=sec/'compiled';out.mkdir(exist_ok=True)
 (out/'sepolicy').write_bytes(policy);lb=(json.dumps(plan,indent=2)+'\n').encode();(out/'labels.json').write_bytes(lb)
 pin={'schema':'S7-NATIVE-SELINUX-1','policy_version':30,'sha256':sha(policy),'bytes':len(policy),
 'labels_sha256':sha(lb),'labels_bytes':len(lb),'runtime_sha256':plan['runtime_sha256'],
 'enforcing_required':True,'reload_existing_policy':False,'new_domains_permissive':False}
 (out/'PIN.json').write_text(json.dumps(pin,indent=2)+'\n')
 report={'original_CIL_hashes_verified':True,'original_Android_to_Android_grants_added':False,
 'source_permissive_removed':removed,'android_neverallow_assertions':'NOT passed; supplied full-Android vendor policy has conflicts; compiler -N semantics used, no runtime permissive switch',
 'appliance_overlay_sha256':sha(overlay.encode()),'policy_sha256':sha(policy),'labels':len(labels),
 'global_policy_first_load_only':True,'loaded_on_host_or_phone':False}
 from native_policy_checks import verify
 report['appliance_decisions']=verify(out/'sepolicy',out/'labels.json')
 (out/'BUILD.json').write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(pin,indent=2))
 return pin
if __name__=='__main__':build()
