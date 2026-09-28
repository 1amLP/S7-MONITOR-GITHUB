#!/usr/bin/env python3
"""Cross-build only the read-only package checker. NOT a WDK/MediaFoundation build."""
from __future__ import annotations
import hashlib, json, os, struct, subprocess, tempfile
from pathlib import Path
ROOT=Path(__file__).resolve().parents[1]
MODULE=ROOT/'host-windows/packagecheck'
def sha(data:bytes)->str:return hashlib.sha256(data).hexdigest()
def metadata(data:bytes)->dict:
    if len(data)<64 or data[:2]!=b'MZ':raise ValueError('not DOS/PE')
    offset=struct.unpack_from('<I',data,60)[0]
    if offset+96>len(data) or data[offset:offset+4]!=b'PE\0\0':raise ValueError('invalid PE')
    machine=struct.unpack_from('<H',data,offset+4)[0]
    if struct.unpack_from('<H',data,offset+24)[0]!=0x20b:raise ValueError('not PE32+')
    security=struct.unpack_from('<H',data,offset+24+70)[0]
    if security&0x140!=0x140:raise ValueError('ASLR/DEP missing')
    return {'machine':hex(machine),'ASLR':True,'DEP':True}
def main()->None:
    build=MODULE/'build';build.mkdir(exist_ok=True)
    env=os.environ|{'GOTOOLCHAIN':'local','GOPROXY':'off','CGO_ENABLED':'0','GOOS':'windows','GOMAXPROCS':'2'}
    receipt={'schema':'S7_WINDOWS_PREFLIGHT_BUILD_1','scope':'read-only package checker only',
             'windows_executed':False,'wdk_build_executed':False,'signed':False,'installed':False,'files':[]}
    for target,goarch,machine in [('x64','amd64','0x8664'),('arm64','arm64','0xaa64')]:
        folder=build/target;folder.mkdir(exist_ok=True)
        path=folder/'S7PackageCheck.exe'
        cmd=['go','build','-trimpath','-ldflags=-s -w -buildid=','-o',str(path),'./cmd/s7-packagecheck']
        subprocess.run(cmd,cwd=MODULE,env=env|{'GOARCH':goarch},check=True,timeout=120)
        a=path.read_bytes();info=metadata(a)
        if info['machine']!=machine:raise ValueError('wrong target architecture')
        with tempfile.TemporaryDirectory(prefix='s7-checker-repeat-') as tmp:
            second=Path(tmp)/path.name;cmd[5]=str(second)
            subprocess.run(cmd,cwd=MODULE,env=env|{'GOARCH':goarch},check=True,timeout=120)
            if a!=second.read_bytes():raise ValueError('non-reproducible checker build')
        receipt['files'].append({'path':path.relative_to(ROOT).as_posix(),'bytes':len(a),'sha256':sha(a),
                                'architecture':target,'reproducible':True,**info})
    receipt['go_version']=subprocess.check_output(['go','version'],text=True).strip()
    source=hashlib.sha256()
    for p in sorted(MODULE.rglob('*')):
        if p.is_file() and 'build' not in p.relative_to(MODULE).parts:
            source.update(p.relative_to(MODULE).as_posix().encode()+b'\0'+p.read_bytes()+b'\0')
    receipt['module_source_sha256']=source.hexdigest()
    (build/'BUILD.json').write_text(json.dumps(receipt,indent=2)+'\n')
    print(json.dumps(receipt,indent=2))
if __name__=='__main__':main()
