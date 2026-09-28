#!/usr/bin/env python3
"""Build the actual AMediaCodec worker against the supplied Bionic libc/libdl.

Media API calls are resolved at runtime from the supplied ORIGINAL libmediandk.
This does not implement MediaCodec itself or start Android native media services.
The much larger Android media runtime is not implicitly added to the small BOOT.
"""
from __future__ import annotations
import argparse, hashlib, json, os, re, shutil, subprocess
from pathlib import Path
ROOT=Path(__file__).resolve().parents[1]
OTA_SHA='e9885e0e3e895ee759128b8dfa9097b749de13c115fb9e3548d303d8c9ba9e4b'
FILES={
 'lib64/libmediandk.so':'lib64/libmediandk.so',
 'lib64/libOMX.Exynos.AVC.Decoder.so':'vendor/lib64/omx/libOMX.Exynos.AVC.Decoder.so',
 'lib64/libOMX.Exynos.AVC.Encoder.so':'vendor/lib64/omx/libOMX.Exynos.AVC.Encoder.so',
 'etc/media_codecs.xml':'vendor/etc/media_codecs.xml',
 'etc/media-omx.rc':'vendor/etc/init/android.hardware.media.omx@1.0-service.rc',
}
def sha(p):return hashlib.sha256(p.read_bytes()).hexdigest()
def main():
 p=argparse.ArgumentParser(description=__doc__);p.add_argument('--system-root',type=Path);p.add_argument('--clang',default='clang');a=p.parse_args()
 out=ROOT/'hardware/mediacodec';vendor=out/'vendor';vendor.mkdir(parents=True,exist_ok=True)
 receipt=out/'VENDOR_INPUTS.json'
 if a.system_root:
  if receipt.exists():raise ValueError('vendor originals already pinned; no overwrite')
  entries=[]
  for dest,src in FILES.items():
   source=a.system_root/src
   if source.is_symlink() or not source.is_file():raise ValueError('missing regular source: '+str(source))
   target=vendor/dest;target.parent.mkdir(parents=True,exist_ok=True);shutil.copyfile(source,target)
   entries.append(dict(path=dest,source='SYSTEM/system/'+src,bytes=target.stat().st_size,sha256=sha(target)))
  receipt.write_text(json.dumps(dict(schema='S7-MEDIACODEC-VENDOR-1',ota_sha256=OTA_SHA,files=entries,
       original_bytes_unmodified=True,full_runtime_in_boot=False,media_services_started=False),indent=2)+'\n')
 manifest=json.loads(receipt.read_text())
 if manifest['ota_sha256']!=OTA_SHA or {x['path'] for x in manifest['files']}!=set(FILES):raise ValueError('wrong vendor base')
 for v in manifest['files']:
  path=vendor/v['path']
  if path.is_symlink() or sha(path)!=v['sha256'] or path.stat().st_size!=v['bytes']:raise ValueError('vendor input changed: '+v['path'])
 import xml.etree.ElementTree as ET
 xml=ET.parse(vendor/'etc/media_codecs.xml').getroot()
 for kind,name in [('Decoders','OMX.Exynos.avc.dec'),('Encoders','OMX.Exynos.AVC.Encoder')]:
  if not any(v.get('name')==name and v.get('type')=='video/avc' for v in xml.find(kind)):raise ValueError('unexpected codec name')
 lib=ROOT/'hardware/sensorhub/vendor/lib64'
 output=out/'bin/mediacodec-bridge';output.parent.mkdir(exist_ok=True)
 cmd=[a.clang,'--target=aarch64-linux-android28','-fuse-ld=lld','-nostdlib','-ffreestanding','-fPIE','-pie','-O2',
      '-fno-stack-protector','-mno-outline-atomics','-Wall','-Wextra','-Werror',
      '-Wl,--no-undefined,--build-id=none,-z,relro,-z,now,-e,_start',
      '-Wl,--dynamic-linker,/opt/s7-hub/bin/linker64',
      str(ROOT/'native/support/mediacodec/entry.S'),str(ROOT/'native/support/mediacodec/bridge.c'),
      '-L'+str(lib),'-l:libc.so','-l:libdl.so','-o',str(output)]
 result=subprocess.run(cmd,text=True,capture_output=True,env=os.environ|{'LC_ALL':'C'})
 (out/'BUILD_LOG.json').write_text(json.dumps(dict(argv=cmd,returncode=result.returncode,stdout=result.stdout,stderr=result.stderr),indent=2)+'\n')
 if result.returncode:raise RuntimeError(result.stderr)
 from build_sensorhub import elf_info
 bridge=elf_info(output)
 if set(bridge['needed'])!={'libc.so','libdl.so'}:raise ValueError('unexpected bridge imports')
 info={x:elf_info(vendor/x) for x in FILES if x.endswith('.so')}
 # dlsym has no static linker diagnostics: require every called API in the
 # pinned original library, not only in the host fake used by boundary tests.
 source=(ROOT/'native/support/mediacodec/bridge.c').read_text()
 required=sorted(set(re.findall(r'LOAD\(\w+,"(AMedia\w+)"\)',source)))
 if len(required)<20:raise ValueError('MediaCodec API imports were not enumerated')
 missing=set(required)-set(info['lib64/libmediandk.so']['default_exports'])
 if missing:raise ValueError('pinned libmediandk lacks required API: '+repr(sorted(missing)))
 record=dict(schema='S7-MEDIACODEC-BRIDGE-1',path='bin/mediacodec-bridge',sha256=sha(output),bytes=output.stat().st_size,
     mode=0o755,compiled=True,hardware_tested=False,original_ndk_api=True,codec_components=['OMX.Exynos.avc.dec','OMX.Exynos.AVC.Encoder'],
     dependencies=bridge['needed'],required_api_library='/system/lib64/libmediandk.so',full_runtime_supplied=(out/'runtime/BUNDLE.json').is_file(),
     runtime_in_boot=False,ipc_version=3,shared_nv12_memfd=True,shared_pixel_slots_per_role=1,raw_pixels_in_socket=False,binder_services_required=True,required_api_exports=required,
     per_buffer_output_format=True,native_crop_rect=True,linear_color_formats=[19,21],
     encoder_explicit_baseline_level=True,encoder_fragment_bytes=4<<20,encoder_fragment_parts=64,
     encoder_public_rates=[30,60],encoder_engineering_usb_rates=[120,240],encoder_trial_open_flag=0x100,
     trial_deadline_owner='native camera session context, maximum 30 seconds')
 (out/'BRIDGE_MANIFEST.json').write_text(json.dumps(record,indent=2)+'\n')
 (out/'MEDIA_DEPENDENCIES.json').write_text(json.dumps(dict(scope='direct imports only, NOT a closed runnable media runtime',
     services_rc='vendor/etc/media-omx.rc',files={k:{'needed':v['needed'],'soname':v['soname']} for k,v in info.items()},
     full_runtime_in_boot=False,android_framework_started=False,hardware_tested=False),indent=2)+'\n')
 print(json.dumps(record,indent=2))
if __name__=='__main__':main()
