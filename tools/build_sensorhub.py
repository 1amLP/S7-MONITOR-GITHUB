#!/usr/bin/env python3
"""Build isolated native lhd support. No device access, shell, mount or Android service.

Vendor files are copied from the user's reconstructed SYSTEM once; later builds
use the checked-in originals. Three explicit ABI adapters replace service-linked
libutils/liblog/libhardware_legacy. All strong ELF imports must resolve.
"""
from __future__ import annotations
import argparse, hashlib, json, os, re, shutil, subprocess
from pathlib import Path

ROOT=Path(__file__).resolve().parents[1]
BASE_SHA='e9885e0e3e895ee759128b8dfa9097b749de13c115fb9e3548d303d8c9ba9e4b'
LHD_SHA='22075698ef8f3ee761a8a88558bce9483a021fc4c6eb032d79b1c493c1887c49'
CONFIG_SHA='c40a26d1b3d981fe0e746aef094c1848b7f7e8559c545714a6fa173c50176eb4'
COPIES={
 'bin/lhd':'vendor/bin/hw/lhd', 'bin/linker64':'bin/bootstrap/linker64',
 'lib64/libc.so':'lib64/bootstrap/libc.so', 'lib64/libdl.so':'lib64/bootstrap/libdl.so',
 'lib64/libm.so':'lib64/bootstrap/libm.so', 'lib64/libc++.so':'lib64/libc++.so',
 'source-lhd.conf':'vendor/etc/sensor/lhd.conf',
}
def sha(p):return hashlib.sha256(p.read_bytes()).hexdigest()
def elf_info(p):
 header=subprocess.run(['readelf','-Wh',str(p)],check=True,text=True,capture_output=True,env=os.environ|{'LC_ALL':'C'}).stdout
 if 'AArch64' not in header or 'ELF64' not in header or "little endian" not in header:raise ValueError('not AArch64: '+str(p))
 dynamic=subprocess.run(['readelf','-Wd',str(p)],check=True,text=True,capture_output=True,env=os.environ|{'LC_ALL':'C'}).stdout
 needed=re.findall(r'\(NEEDED\).*?\[(.*?)\]',dynamic)
 sn=re.findall(r'\(SONAME\).*?\[(.*?)\]',dynamic)
 exported=set();imports=set();weak=set();versioned_exports=set();default_exports=set();versioned_imports=[]
 versions=subprocess.run(['readelf','-WV',str(p)],check=True,text=True,capture_output=True,env=os.environ|{'LC_ALL':'C'}).stdout
 version_needs={};owner=None;in_needs=False
 for line in versions.splitlines():
  if line.startswith('Version needs section'):in_needs=True;continue
  if not in_needs:continue
  match=re.search(r'File: (\S+)',line)
  if match:owner=match.group(1)
  match=re.search(r'Name: (\S+).*?Version: (\d+)',line)
  if match:
   if not owner:raise ValueError('version requirement without provider: '+str(p))
   index=int(match.group(2))
   if index in version_needs:raise ValueError('duplicate version requirement index: '+str(p))
   version_needs[index]=(owner,match.group(1))
 table=subprocess.run(['readelf','-W','--dyn-syms',str(p)],check=True,text=True,capture_output=True,env=os.environ|{'LC_ALL':'C'}).stdout
 for line in table.splitlines():
  fields=line.split()
  if len(fields)<8 or not fields[0].rstrip(':').isdigit():continue
  binding=next((i for i,t in enumerate(fields) if t in ('GLOBAL','WEAK','LOCAL')),None)
  if binding is None or len(fields)<binding+4:continue
  bind,vis,idx,symbol=fields[binding:binding+4];name=symbol.split('@',1)[0]
  if idx=='UND':
   (weak if bind=='WEAK' else imports).add(name)
   if '@' in symbol:
    version=symbol.split('@',1)[1]
    match=re.search(r'\((\d+)\)\s*$',line)
    if not match or int(match.group(1)) not in version_needs:raise ValueError('missing version requirement for '+symbol)
    provider,required=version_needs[int(match.group(1))]
    if required!=version:raise ValueError('inconsistent version table for '+symbol)
    versioned_imports.append(dict(name=name,version=version,provider=provider,weak=bind=='WEAK'))
  elif bind in ('GLOBAL','WEAK') and vis in ('DEFAULT','PROTECTED'):
   exported.add(name)
   if '@' in symbol:versioned_exports.add((name,symbol.lstrip('@').split('@')[-1]))
   if '@' not in symbol or '@@' in symbol:default_exports.add(name)
 return dict(needed=needed,soname=sn[0] if sn else p.name,exports=sorted(exported),imports=sorted(imports),weak=sorted(weak),
             default_exports=sorted(default_exports),versioned_exports=sorted(versioned_exports),versioned_imports=versioned_imports)

def audit(directory):
 files={p.relative_to(directory).as_posix():elf_info(p) for p in [directory/'bin/lhd',directory/'bin/linker64',*sorted((directory/'lib64').glob('*.so'))]}
 return audit_info(files)

def audit_info(files):
 """Check the pinned loader closure, including version and provider identity.

 This is an offline consistency check, not an emulator or a relocation test.
 A same-named symbol from another library/version does not satisfy the declared
 version requirement. Weak unresolved imports are reported, never counted as
 proven usable. Loader hooks are mandatory for this particular Bionic runtime.
 """
 providers={}
 for name,v in files.items():
  if v['soname'] in providers:raise ValueError('ambiguous SONAME: '+v['soname'])
  providers[v['soname']]=(name,v)
 defaults=set().union(*(set(v['default_exports']) for v in files.values()))
 resolved=[];weak_missing=[]
 for name,v in files.items():
  missing=set(v['needed'])-providers.keys()
  if missing:raise ValueError(f'{name}: missing DT_NEEDED: {sorted(missing)}')
  if any(s in n.lower() for n in v['needed'] for s in ('binder','hidl','camera','sensorservice')):raise ValueError('service dependency: '+name)
  versioned={x['name'] for x in v['versioned_imports']}
  unresolved=(set(v['imports'])-versioned)-defaults
  if unresolved:raise ValueError(f'{name}: unresolved strong symbols: {sorted(unresolved)}')
  for symbol in v['versioned_imports']:
   target=providers.get(symbol['provider'])
   ok=target is not None and symbol['provider'] in v['needed'] and (symbol['name'],symbol['version']) in set(map(tuple,target[1]['versioned_exports']))
   item=dict(requester=name,**symbol)
   if not ok:
    if symbol['weak']:weak_missing.append(item)
    else:raise ValueError(f"{name}: unresolved version/provider: {symbol['name']}@{symbol['version']} from {symbol['provider']}")
   else:resolved.append(item)
  for symbol in sorted((set(v['weak'])-versioned)-defaults):
   if symbol.startswith('__loader_'):raise ValueError(f'{name}: missing Bionic loader hook: {symbol}')
   weak_missing.append(dict(requester=name,name=symbol,weak=True))
 # The manifest cannot hide a disconnected library that happened to export a
 # missing name. Both roots are explicit: the program and its ELF interpreter.
 reached=set();todo=['bin/lhd','bin/linker64']
 while todo:
  name=todo.pop()
  if name in reached:continue
  if name not in files:raise ValueError('missing ELF root: '+name)
  reached.add(name);todo.extend(providers[n][0] for n in files[name]['needed'])
 if reached!=set(files):raise ValueError('orphan ELF files: '+repr(sorted(set(files)-reached)))
 return {'scope':'static ELF dependency, symbol-version/provider and loader-hook closure; NOT execution, relocation or hardware proof',
         'files':{k:{a:v[a] for a in ('needed','soname','imports','weak','versioned_imports')} for k,v in files.items()},
         'versioned_imports_resolved':resolved,'unresolved_weak_imports':weak_missing,
         'loader_roots':['bin/lhd','bin/linker64'],
         'missing_strong_symbols':[], 'android_services_required_by_dt_needed':False}

def main():
 p=argparse.ArgumentParser();p.add_argument('--system-root',type=Path);p.add_argument('--clang',default='clang');a=p.parse_args()
 out=ROOT/'hardware/sensorhub'; vendor=out/'vendor'; run=out/'runtime';out.mkdir(exist_ok=True)
 if a.system_root:
  if vendor.exists():raise ValueError('vendor originals already exist; no overwrite')
  for dest,src in COPIES.items():
   inp=a.system_root/src
   if not inp.is_file() or inp.is_symlink():raise ValueError('regular source required: '+str(inp))
   target=vendor/dest;target.parent.mkdir(parents=True,exist_ok=True);shutil.copyfile(inp,target)
 if sha(vendor/'bin/lhd')!=LHD_SHA or sha(vendor/'source-lhd.conf')!=CONFIG_SHA:raise ValueError('wrong lhd/config base')
 original_manifest=out/'VENDOR_INPUTS.json'
 entries={k:{'source':'SYSTEM/system/'+v,'sha256':sha(vendor/k),'bytes':(vendor/k).stat().st_size} for k,v in COPIES.items()}
 record={'base_ota_sha256':BASE_SHA,'files':entries}
 if original_manifest.exists() and json.loads(original_manifest.read_text())!=record:raise ValueError('vendor originals differ from pinned extraction')
 original_manifest.write_text(json.dumps(record,indent=2)+'\n')
 if run.exists():shutil.rmtree(run)
 run.mkdir()
 for dest in COPIES:
  if dest=='source-lhd.conf':continue
  target=run/dest;target.parent.mkdir(parents=True,exist_ok=True);shutil.copyfile(vendor/dest,target)
 buildlog=[]
 for name,src in [('liblog.so','log.c'),('libutils.so','clock.c'),('libhardware_legacy.so','wake.c')]:
  cmd=[a.clang,'--target=aarch64-linux-android28','-fuse-ld=lld','-shared','-fPIC','-ffreestanding','-mno-outline-atomics','-O2','-fno-stack-protector','-nostdlib',
       '-Wall','-Wextra','-Werror','-Wl,--no-undefined','-Wl,--build-id=none','-Wl,-z,relro,-z,now',f'-Wl,-soname,{name}',
       str(ROOT/'native/support/lhd'/src),'-L'+str(run/'lib64'),'-l:libc.so','-o',str(run/'lib64'/name)]
  result=subprocess.run(cmd,text=True,capture_output=True)
  buildlog.append({'argv':cmd,'returncode':result.returncode,'stdout':result.stdout,'stderr':result.stderr})
  if result.returncode:raise RuntimeError(result.stderr)
 # Only storage/failsafe paths are changed; chip power/transport commands stay vendor-defined.
 config=(vendor/'source-lhd.conf').read_text(); lines=config.splitlines()
 substitutions={'NvStorageDir':'/run/s7-sensorhub/','LogDirectory':'/run/s7-sensorhub/', 'LheFailSafe':'LOG'}
 lines=[(k+'='+substitutions[k]) if (k:=line.split('=',1)[0]) in substitutions and '=' in line else line for line in lines]
 config='\n'.join(lines)+'\n';(run/'lhd.conf').write_text(config)
 report=audit(run)
 manifest={'schema':'S7_SENSORHUB_RUNTIME_1','prefix':'opt/s7-hub','lhd_sha256':LHD_SHA,'hardware_accepted':False,
           'android_services':[], 'uses_bionic_libraries':True,
           'files':[{'path':f.relative_to(run).as_posix(),'sha256':sha(f),'bytes':f.stat().st_size,
                     'mode':0o755 if f.parent.name=='bin' else 0o644}
                    for f in sorted(run.rglob('*')) if f.is_file()]}
 (out/'RUNTIME_MANIFEST.json').write_text(json.dumps(manifest,indent=2)+'\n')
 (out/'ELF_AUDIT.json').write_text(json.dumps(report,indent=2)+'\n')
 (out/'BUILD_LOG.json').write_text(json.dumps(buildlog,indent=2)+'\n')
 print(json.dumps({'files':len(manifest['files']),'bytes':sum(v['bytes'] for v in manifest['files']),
                   'static_elf_closure':'PASS','runtime_executed':False}))
if __name__=='__main__':main()
