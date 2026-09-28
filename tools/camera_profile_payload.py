"""Admission of the exact generated native camera plan set for the 481 kernel.
This allowlist is a build-integrity boundary, not a hardware acceptance flag.
"""
import hashlib,json
PROFILE_SHA256='3a97dee25c8e0d95569795b378dc7a24bc4fdea9c9cb8256aaec3d48e6964085'
HAL_SHA256='9604961bf7e4e8c657b70185f359cb87c368769eb71d24ea6d4b58f5ac8045f3'
KERNEL_ABI='herolte-3.18.140-481bdb278a10'

def validate_payload(data,firmware_dir):
    if len(data)>256*1024 or hashlib.sha256(data).hexdigest()!=PROFILE_SHA256:
        raise ValueError('camera plans differ from generated, source-pinned release; regenerate and review before updating admission')
    profiles=json.loads(data)
    if profiles.get('schema')!='S7_NATIVE_CAMERA_PROFILES_1' or profiles.get('source_sha256')!=HAL_SHA256:
        raise ValueError('camera profile schema/HAL mismatch')
    if len(profiles.get('plans',[]))!=17:raise ValueError('incomplete native profile matrix')
    checked={}
    for plan in profiles['plans']:
        if plan['kernel_abi']!=KERNEL_ABI:raise ValueError('camera plan kernel mismatch')
        for pin in plan['firmware']:
            name=pin['name'];path=firmware_dir/name
            if path.name!=name or path.is_symlink() or not path.is_file():raise ValueError('invalid camera firmware '+name)
            if name not in checked:checked[name]=hashlib.sha256(path.read_bytes()).hexdigest()
            if checked[name]!=pin['sha256']:raise ValueError('camera firmware checksum '+name)
    return profiles
