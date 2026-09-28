#!/usr/bin/env python3
"""Build an external, pinned native-media root from the supplied OTA exports.

No Android framework/ART/APK, boot-image growth, device mount or flash. Selected
Android init runs only SECOND STAGE in a private PID/mount namespace, with our
single rc. It supplies the real Bionic property area and native service startup.
The host continues to use our Go PID1. ARM32 OMX/mediaserver are retained as ARM32.
"""
from __future__ import annotations
import argparse, collections, gzip, hashlib, json, os, re, shutil, tarfile
from pathlib import Path, PurePosixPath
import subprocess
from media_sandbox import complete as complete_sandbox
from media_hal import EXTRA_ROOTS, validate as validate_hal
ROOT=Path(__file__).resolve().parents[1]
OTA_SHA='e9885e0e3e895ee759128b8dfa9097b749de13c115fb9e3548d303d8c9ba9e4b'

def digest(p):
 h=hashlib.sha256()
 with p.open('rb') as f:
  for chunk in iter(lambda:f.read(1<<20),b''):h.update(chunk)
 return h.hexdigest()
def main():
 ap=argparse.ArgumentParser(description=__doc__)
 ap.add_argument('--export',type=Path,required=True);ap.add_argument('--metadata',type=Path,required=True)
 ap.add_argument('--apex',type=Path,required=True);ap.add_argument('--stage',type=Path,required=True)
 a=ap.parse_args(); source=a.export;stage=a.stage
 if stage.exists():raise ValueError('fresh stage required')
 stage.mkdir(parents=True)
 links={}
 for line in a.metadata.read_text().splitlines():
  if line.startswith('link\t'):
   _,name,target=line.split('\t');links[name]=target
 for apex in ('com.android.runtime','com.android.i18n','com.android.media','com.android.os.statsd','com.android.neuralnetworks','com.android.media.swcodec'):
  for line in (a.apex/(apex+'.tsv')).read_text().splitlines():
   if line.startswith('link\t'):
    _,name,target=line.split('\t');links['/apex/'+apex+name]=target
 def origin(name):
  # Resolve only source metadata, never follow a host symlink outside the export.
  for _ in range(32):
   parts=PurePosixPath(name).parts;found=False
   for n in range(2,len(parts)+1):
    head=str(PurePosixPath(*parts[:n]))
    if head not in links:continue
    v=links[head];name=os.path.normpath((v if v.startswith('/') else str(PurePosixPath(head).parent/v))+'/'+str(PurePosixPath(*parts[n:])))
    found=True;break
   if not found:break
  else:raise ValueError('source link cycle: '+name)
  if name.startswith('/apex/'):
   parts=PurePosixPath(name).parts
   return a.apex/parts[2]/Path(*parts[3:]), name
  return source/name.lstrip('/'),name
 info={};records={};edges=[];todo=collections.deque()
 def elf(p):
  if p not in info:
   with p.open('rb') as f:header=f.read(20)
   if header[:4]!=b'\x7fELF':info[p]=None;return None
   result=subprocess.run(['readelf','-d','-l',str(p)],text=True,capture_output=True,check=True).stdout
   needs=re.findall(r'\(NEEDED\).*?\[(.*?)\]',result)
   interp=re.search(r'Requesting program interpreter: (.*?)\]',result)
   info[p]=(64 if header[4]==2 else 32,needs,interp.group(1) if interp else None)
  return info[p]
 def add(name,why,source_name=None):
  name=name.lstrip('/')
  if name in records:return
  if any(x in name.split('/') for x in ('..','')):raise ValueError(name)
  p,canonical=origin('/'+(source_name or name))
  if not p.is_file() or p.is_symlink():raise ValueError('missing selected source '+str(p))
  data=elf(p);out=stage/name;out.parent.mkdir(parents=True,exist_ok=True);shutil.copyfile(p,out)
  mode=0o755 if data and (data[2] or "/bin/" in name) else 0o644;out.chmod(mode)
  records[name]={'path':name,'source':canonical,'bytes':out.stat().st_size,'sha256':digest(out),'mode':mode,'reason':why}
  if data:todo.append((name,p,data))
 seeds=['system/bin/init','system/bin/servicemanager','system/system_ext/bin/hwservicemanager',
        'system/system_ext/bin/hw/android.hidl.allocator@1.0-service','system/bin/mediaserver',
        'system/bin/logd','system/bin/service','vendor/bin/vndservicemanager','vendor/bin/hw/android.hardware.media.omx@1.0-service',
        'vendor/bin/hw/android.hardware.graphics.allocator@2.0-service','system/lib64/libmediandk.so']
 for name in seeds:add(name,'native service/client root')
 for name in EXTRA_ROOTS:add(name,'selected HIDL allocator implementation and registration client')
 for lib in ('lib','lib64'):
  for name in ['vendor/'+lib+'/libstagefrighthw.so','vendor/'+lib+'/libExynosOMX_Core.so','vendor/'+lib+'/omx/libOMX.Exynos.AVC.Decoder.so',
               'vendor/'+lib+'/omx/libOMX.Exynos.AVC.Encoder.so','vendor/'+lib+'/hw/gralloc.exynos5.so',
               'vendor/'+lib+'/hw/android.hardware.graphics.mapper@2.0-impl-2.1.so',
               'system/'+lib+'/hw/android.hidl.memory@1.0-impl.so','vendor/'+lib+'/hw/android.hidl.memory@1.0-impl.so',
               'system/'+lib+'/libstagefright_omx.so']:
   add(name,'explicit dlopen hardware/plugin root')
 # Android init bootstrap linker and normal service linker are both required.
 for name in ['system/bin/bootstrap/linker','system/bin/bootstrap/linker64','system/bin/linker','system/bin/linker64']:
  add(name,'ELF interpreter')
 while todo:
  name,p,(bits,needs,interp)=todo.popleft();lib='lib64' if bits==64 else 'lib'
  vendor=name.startswith('vendor/')
  search=([f'vendor/{lib}',f'vendor/{lib}/hw',f'vendor/{lib}/omx'] if vendor else [])+[
    f'system/{lib}',f'system/{lib}/bootstrap',f'system/system_ext/{lib}',
    f'apex/com.android.runtime/{lib}/bionic',f'apex/com.android.i18n/{lib}',f'apex/com.android.media/{lib}',f'apex/com.android.os.statsd/{lib}',f'apex/com.android.neuralnetworks/{lib}',f'apex/com.android.media.swcodec/{lib}']
  for need in needs:
   candidates=[]
   for d in search:
    dest=d+'/'+need;f,_=origin('/'+dest)
    if f.is_file() and elf(f) and elf(f)[0]==bits:candidates.append(dest)
   if not candidates:raise ValueError(f'unresolved ELF{bits} {need} from {name}')
   dep=candidates[0];add(dep,'DT_NEEDED from '+name);edges.append({'from':name,'needed':need,'to':dep,'elf_bits':bits})
  if interp:add(interp,'PT_INTERP from '+name)
 # Original config inputs. Only our generated init rc is installed: never import
 # the OTA init tree (which would start Android, USB takeover, thermal/power HALs).
 configs=['vendor/etc/media_codecs.xml','vendor/etc/media_codecs_performance.xml',
  'vendor/etc/media_profiles_V1_0.xml','vendor/etc/vintf/manifest.xml','system/etc/vintf/manifest.xml',
  'system/etc/selinux/plat_file_contexts','vendor/etc/selinux/vendor_file_contexts','system/system_ext/etc/selinux/system_ext_file_contexts','system/etc/selinux/plat_property_contexts','system/etc/selinux/plat_service_contexts',
  'system/etc/selinux/plat_hwservice_contexts','vendor/etc/selinux/vendor_property_contexts',
  'vendor/etc/selinux/vendor_service_contexts','vendor/etc/selinux/vendor_hwservice_contexts','vendor/etc/selinux/vndservice_contexts',
  'system/system_ext/etc/selinux/system_ext_property_contexts','system/system_ext/etc/selinux/system_ext_service_contexts',
  'system/system_ext/etc/selinux/system_ext_hwservice_contexts','system/etc/task_profiles.json','system/etc/cgroups.json','system/system_ext/etc/vintf/manifest/android.hidl.allocator@1.0-service.xml']
 for name in configs:
  if origin('/'+name)[0].is_file():add(name,'native media config/property context')
 for tree in ['system/etc/seccomp_policy','vendor/etc/seccomp_policy']:
  base=origin('/'+tree)[0]
  if base.exists():
   for p in sorted(base.glob('*')):
    if p.is_file() and any(x in p.name for x in ('media','omx','codec','base')):add(tree+'/'+p.name,'original native sandbox policy')
 complete_sandbox(stage,records,lambda name:add(name,'original sandbox @include'))
 sandbox=complete_sandbox(stage,records,lambda name:add(name,'original sandbox @include'))
 # Resolve recursive XML includes instead of silently relying on absent configs.
 import xml.etree.ElementTree as ET
 pending=[n for n in records if 'media_codecs' in n and n.endswith('.xml')];seen=set()
 while pending:
  n=pending.pop()
  if n in seen:continue
  seen.add(n)
  for el in ET.parse(stage/n).getroot().iter('Include'):
   name=str(PurePosixPath(n).parent/el.attrib['href']);add(name,'codec XML include');pending.append(name)
 # ICU data is a runtime file, not a DT_NEEDED library.
 for p in sorted((a.apex/'com.android.i18n').rglob('*.dat')):
  add('apex/com.android.i18n/'+str(p.relative_to(a.apex/'com.android.i18n')),'ICU runtime data')
 def generated(name,text):
  out=stage/name;out.parent.mkdir(parents=True,exist_ok=True);out.write_text(text);out.chmod(0o644)
  records[name]={'path':name,'source':'generated:tools/build_media_runtime.py','bytes':out.stat().st_size,'sha256':digest(out),'mode':0o644,'reason':'isolated native-only configuration'}
 # Build properties are not copied wholesale: USB, network, camera activation and
 # thermal policy remain owned by the outer PID1, not the retained media services.
 props={}
 keys=('ro.product.','ro.build.version.','ro.vendor.build.version.','ro.vndk.','ro.treble.','ro.board.','ro.hardware','media.','vendor.media.')
 for base in ['system/build.prop','vendor/build.prop']:
  for line in origin('/'+base)[0].read_text().splitlines():
   if '=' in line and line.startswith(keys):k,v=line.split('=',1);props[k]=v
 props.update({'ro.hardware':'samsungexynos8890','ro.hardware.gralloc':'exynos5','ro.boot.hardware':'samsungexynos8890',
 'ro.debuggable':'0','ro.secure':'1','ro.build.type':'user','ro.boot.init_rc':'/system/etc/init/hw/init.rc',
 'debug.stagefright.ccodec':'0','ro.config.low_ram':'false'})
 generated('system/etc/prop.default','\n'.join(k+'='+v for k,v in sorted(props.items()))+'\n')
 generated('system/build.prop',f'ro.build.version.sdk=34\nro.build.version.release=14\nro.product.first_api_level=23\n')
 generated('linkerconfig/ld.config.txt','''dir.system = /system/bin
dir.system_ext = /system/system_ext/bin
dir.vendor = /vendor/bin
dir.codec = /s7
[system]
namespace.default.isolated = false
namespace.default.search.paths = /system/${LIB}:/system/${LIB}/bootstrap:/system/system_ext/${LIB}:/apex/com.android.runtime/${LIB}/bionic:/apex/com.android.i18n/${LIB}:/apex/com.android.media/${LIB}:/apex/com.android.os.statsd/${LIB}:/apex/com.android.neuralnetworks/${LIB}:/apex/com.android.media.swcodec/${LIB}
namespace.default.asan.search.paths = /system/${LIB}
[system_ext]
namespace.default.isolated = false
namespace.default.search.paths = /system/${LIB}:/system/${LIB}/bootstrap:/system/system_ext/${LIB}:/apex/com.android.runtime/${LIB}/bionic:/apex/com.android.i18n/${LIB}:/apex/com.android.media/${LIB}:/apex/com.android.os.statsd/${LIB}:/apex/com.android.neuralnetworks/${LIB}:/apex/com.android.media.swcodec/${LIB}
[vendor]
namespace.default.isolated = false
namespace.default.search.paths = /vendor/${LIB}:/vendor/${LIB}/hw:/vendor/${LIB}/omx:/system/${LIB}:/system/${LIB}/bootstrap:/system/system_ext/${LIB}:/apex/com.android.runtime/${LIB}/bionic:/apex/com.android.i18n/${LIB}:/apex/com.android.media/${LIB}:/apex/com.android.os.statsd/${LIB}:/apex/com.android.neuralnetworks/${LIB}:/apex/com.android.media.swcodec/${LIB}
[codec]
namespace.default.isolated = false
namespace.default.search.paths = /system/${LIB}:/system/${LIB}/bootstrap:/system/system_ext/${LIB}:/apex/com.android.runtime/${LIB}/bionic:/apex/com.android.i18n/${LIB}:/apex/com.android.media/${LIB}:/apex/com.android.os.statsd/${LIB}:/apex/com.android.neuralnetworks/${LIB}:/apex/com.android.media.swcodec/${LIB}
''')
 generated('system/etc/init/hw/init.rc','''# Native media ONLY. Parsed by the original Android SECOND STAGE init in
# a private PID namespace. No Android boot rc, HAL glob, APEX daemon, Zygote,
# ART, APK, surfaceflinger, audio/camera/power/thermal/USB service is imported.
on early-init
    # Our PID1 has created the complete selected device set before this init.
    # No full Android ueventd/boot scripts or APEX daemon are started.
    wait /dev/.s7-media-devices-ready 1
    mount none /s7/apex-seed /apex bind rec
    mount none /s7/linkerconfig-seed /linkerconfig bind rec
    setprop ro.cold_boot_done true
    mkdir /dev/socket 0755 root root
    mkdir /dev/pts 0755 root root
    mkdir /data/misc 0755 root root
    mkdir /data/misc/media 0770 media media
    mkdir /data/misc/mediadrm 0770 mediadrm mediadrm
    mkdir /data/local 0755 root root
    mkdir /data/local/tmp 0770 shell shell
    mkdir /data/property 0700 root root

on init
    setprop ro.hardware samsungexynos8890
    setprop ro.hardware.gralloc exynos5
    setprop debug.stagefright.ccodec 0
    start logd
    start servicemanager
    start hwservicemanager
    start vndservicemanager

on property:servicemanager.ready=true && property:hwservicemanager.ready=true
    start hidl_memory
    start gralloc
    start media
    start vendor.media.omx
    start s7_media_broker

service logd /system/bin/logd
    user logd
    group logd system readproc
    socket logd stream 0666 logd logd
    socket logdr seqpacket 0666 logd logd
    socket logdw dgram+passcred 0222 logd logd
    disabled
    oneshot

service servicemanager /system/bin/servicemanager
    user system
    group system readproc
    disabled
    oneshot

service hwservicemanager /system/system_ext/bin/hwservicemanager
    user system
    group system readproc
    disabled
    oneshot

# Retain the actual vendor Binder context manager. Do not import its original
# class_restart/shutdown-critical actions into our independent appliance.
service vndservicemanager /vendor/bin/vndservicemanager /dev/vndbinder
    user system
    group system readproc
    disabled
    oneshot

service hidl_memory /system/system_ext/bin/hw/android.hidl.allocator@1.0-service
    user system
    group system
    disabled
    oneshot

service gralloc /vendor/bin/hw/android.hardware.graphics.allocator@2.0-service
    user system
    group graphics
    disabled
    oneshot

service media /system/bin/mediaserver
    user media
    group audio camera inet drmrpc mediadrm
    disabled
    oneshot

service vendor.media.omx /vendor/bin/hw/android.hardware.media.omx@1.0-service
    user mediacodec
    group camera drmrpc mediadrm audio
    disabled
    oneshot

service s7_media_broker /s7/native --media-broker
    user root
    group root
    disabled
    oneshot
''')
 # libvndksupport/HIDL dlopen uses the exported sphal namespace, not only
 # DT_NEEDED. Link back to default for shared platform singletons (Binder/HIDL).
 config=(stage/'linkerconfig/ld.config.txt').read_text()
 blocks=config.split('[')
 extra="""additional.namespaces = sphal
namespace.sphal.isolated = false
namespace.sphal.visible = true
namespace.sphal.search.paths = /vendor/${LIB}:/vendor/${LIB}/hw:/vendor/${LIB}/omx
namespace.sphal.links = default
namespace.sphal.link.default.allow_all_shared_libs = true
"""
 for i in range(1,len(blocks)):blocks[i]+=extra
 generated('linkerconfig/ld.config.txt','['.join(blocks))
 # HIDL uses the canonical /system_ext path; retain the exact allocator fragment.
 # Empty mount/bind targets, never OS block-device nodes or a shell executable.
 for d in ['dev','proc','sys','data','run','tmp','s7','s7-control','mnt','mnt/user','mnt/installer','mnt/androidwritable','apex','bootstrap-apex','linkerconfig','s7/apex-seed','s7/linkerconfig-seed',
 'system/etc/init','system_ext/etc/init','product/etc/init','vendor/etc/init','odm/etc/init']:(stage/d).mkdir(parents=True,exist_ok=True)
 for n in ['s7/native','s7/mediacodec-bridge']:(stage/n).touch()
 # Preserve source identities of selected roots, notably the mixed ARM32/ARM64 set.
 report={'schema':'S7-MEDIA-RUNTIME-1','ota_sha256':OTA_SHA,'original_executables_unmodified':True,
  'android_framework':False,'private_android_second_stage_init':True,'files':list(sorted(records.values(),key=lambda x:x['path'])),
  'elf_edges':edges,'service_elf_bits':{n:elf(origin('/'+n)[0])[0] for n in seeds if '/bin/' in n},
  'unresolved_dt_needed':[],'runtime_executed':False,'dlopen_roots_explicit':True,'sandbox_dependencies':sandbox,
  'media_hal_roots':validate_hal(stage,records)}
 generated('s7/runtime-manifest.json',json.dumps(report,indent=2)+'\n')
 out=ROOT/'hardware/mediacodec/runtime';out.mkdir(parents=True,exist_ok=True)
 tarpath=out/'runtime.tar.gz'
 with tarpath.open('wb') as raw,gzip.GzipFile(filename='',mode='wb',fileobj=raw,mtime=0,compresslevel=6) as gz,tarfile.open(fileobj=gz,mode='w|',format=tarfile.USTAR_FORMAT) as tar:
  for p in sorted(stage.rglob('*')):
   rel=p.relative_to(stage).as_posix();ti=tarfile.TarInfo(rel);ti.uid=ti.gid=0;ti.mtime=0
   ti.mode=0o755 if p.is_dir() else records.get(rel,{}).get('mode',0o644)
   if p.is_dir():ti.type=tarfile.DIRTYPE;tar.addfile(ti)
   else:
    ti.size=p.stat().st_size
    with p.open('rb') as f:tar.addfile(ti,f)
 total=sum(p.stat().st_size for p in stage.rglob('*') if p.is_file())
 pin={'schema':'S7-MEDIA-BUNDLE-1','sha256':digest(tarpath),'bytes':tarpath.stat().st_size,'unpacked_bytes':total,
      'members':sum(1 for _ in stage.rglob('*')),'max_unpacked_bytes':384<<20,'ota_sha256':OTA_SHA}
 (out/'BUNDLE.json').write_text(json.dumps(pin,indent=2)+'\n')
 (out/'CONTENTS.json').write_text(json.dumps(report,indent=2)+'\n')
 print(json.dumps(pin,indent=2));print('ELF edges',len(edges),'files',len(records))
if __name__=='__main__':main()
