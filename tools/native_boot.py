#!/usr/bin/env python3
"""Offline-only native initramfs builder for one pinned S7 BOOT input.

No block-device access, mounts, flashing, network, arbitrary unpack paths, or
commands from archives. Kernel, DTB, addresses and header cmdline are preserved.
"""
from __future__ import annotations
import argparse
from dataclasses import dataclass
import hashlib
import json
import lzma
import os
from pathlib import Path, PurePosixPath
import stat
import struct

PAGE = 2048
CAPACITY = 40 * 1024 * 1024
FOOTER = b'SEANDROIDENFORCE'
INPUT_SHA = '6544f30ceb74fda8be35d55664dd3570bdd5a00f453594ed1d488dab7c54233e'
KERNEL_SHA = '7d73f82eac469c5dc1e141d5ec0f28f208a0c91449ff85acc868f5a1f2dfbc01'
DTB_SHA = '0c180a7249d70e7a4623a7a977a5f552670aa978fe981b992557e77c5349318a'


def sha(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def ui_build_status(gpu_probe: bool, ui_diagnostics: bool = False) -> dict:
    return dict(gpu_probe_in_boot=gpu_probe, gpu_menu_gpu_only=True,
                glass_style='Mali draw-list to double DMA-BUF; independent DECON menu and Preview layers; no CPU fallback',
                glass_live_backdrop=True, glass_hardware_accepted=False,
                glass_hardware_self_test_required=True, glass_snapshot_blur=False,
                direct_fb_userptr_scaler=False, offscreen_userptr_scaler=False,
                monitor_presenter_owned_nv12_frames=0, monitor_presenter_dma_pointers_retained=True,
                camera_cached_nv12_upload=False, camera_padding_only_clear=False,
                camera_compiled_transform_offsets=False, camera_dmabuf_frame_lease=True,
                camera_high_fps_webcam_unlocked=True, camera_high_fps_hardware_accepted=False,
                camera_capture_wait='FIMC queue events; bounded cancellation',
                camera_mcsc_buffers=6,
                herolte_sensor_isp_provider='17 source-derived profiles including rear 720p120/240; measured cadence requires device validation',
                native_camera='shared DMA capture, preview and MFC/WinUSB; high-FPS admission is not hardware acceptance',
                decon_fenced_scanout=True, decon_scanout_buffers=2,
                monitor_direct_dmabuf=True, preview_blank_until_frame=True,
                monitor_direct_nv12_vpp=True, full_rgb_for_diagnostic_capture_only=True,
                settings_schema=15, settings_automatic=True, settings_restore_enabled=True,
                home_action='toggle_menu', recents_action='fullscreen_preview_sensor_else_touch_pad',
                back_fullscreen_action='toggle_preview_monitor', rear_preview_flashlight=True,
                monitor_modes=[[1280,720,60],[2560,1440,60]], monitor_default=[1280,720,60],
                home_link_driver_check='none; automatic USB link; package delivery remains incomplete',
                preview_window_modes=['small', 'fullscreen'],
                fullscreen_camera_white_gpu_controls=True, preview_touch_focus=True,
                preview_blocks_pad=False, transient_gpu_notifications=True,
                cache_policy='serial-pinned CACHE settings; journal recovery allowed; no fsck or format',
                diagnostic_payload_crc32=True,
                glass_background_max_hz=0, glass_pacing_adapts_to_compose_cost=False,
                touchpad_polling_hz=[90], touchpad_polling_default_hz=90,
                home_link_wave_implemented=False, ui_diagnostics=ui_diagnostics)


def align(n: int, unit: int = PAGE) -> int:
    return (n + unit - 1) // unit * unit


def digest(parts: list[bytes]) -> bytes:
    h = hashlib.sha1()
    for p in parts[:3]:
        h.update(p)
        h.update(struct.pack('<I', len(p)))
    if parts[3]:
        h.update(parts[3])
        h.update(struct.pack('<I', len(parts[3])))
    return h.digest()


def extract_pinned_kernel(data: bytes, image_sha: str, kernel_sha: str) -> bytes:
    """Return a raw kernel only from one exact legacy BOOT evidence image."""
    if len(data) != CAPACITY or sha(data) != image_sha or data[:8] != b'ANDROID!':
        raise ValueError('BOOT evidence image mismatch')
    fields = struct.unpack_from('<10I', data, 8)
    if fields[7] != PAGE or fields[1] != 0x10008000 or fields[3] != 0x11000000 or fields[6] != 0x10000100:
        raise ValueError('BOOT evidence header mismatch')
    size = fields[0]
    end = PAGE + align(size)
    if not size or end > len(data) or any(data[PAGE + size:end]):
        raise ValueError('BOOT evidence kernel layout mismatch')
    kernel = data[PAGE:PAGE + size]
    if sha(kernel) != kernel_sha or kernel[56:60] != b'ARMd':
        raise ValueError('BOOT evidence kernel mismatch')
    return kernel


@dataclass
class Boot:
    header: bytes
    parts: list[bytes]
    payload_end: int

    @staticmethod
    def parse(data: bytes, pinned: bool = False) -> 'Boot':
        if not PAGE <= len(data) <= CAPACITY or data[:8] != b'ANDROID!':
            raise ValueError('invalid S7 legacy boot container')
        if pinned and (len(data) != CAPACITY or sha(data) != INPUT_SHA):
            raise ValueError('input differs from the exact user-supplied experimental BOOT')
        fields = struct.unpack_from('<10I', data, 8)
        if fields[7] != PAGE or fields[1] != 0x10008000 or fields[3] != 0x11000000 or fields[6] != 0x10000100:
            raise ValueError('unexpected page size/load addresses')
        sizes = fields[0], fields[2], fields[4], fields[8]
        if not sizes[0] or not sizes[1] or sizes[2] or not sizes[3]:
            raise ValueError('unsupported payload layout')
        offset, parts = PAGE, []
        for n in sizes:
            end = offset + align(n)
            if end > len(data) or any(data[offset + n:end]):
                raise ValueError('truncated payload or nonzero alignment bytes')
            parts.append(data[offset:offset + n])
            offset = end
        if data[offset:offset + len(FOOTER)] != FOOTER:
            raise ValueError('Samsung footer missing')
        # The exact working e418 source carries an opaque Samsung signing tail.
        # Its full-file pin above authenticates that input. A changed ramdisk makes
        # the old signature inapplicable, so generated unlocked-device images use
        # the already hardware-proven SEANDROIDENFORCE footer plus zero padding.
        if not pinned and any(data[offset + len(FOOTER):]):
            raise ValueError('generated footer/padding mismatch')
        if data[576:596] != digest(parts) or any(data[596:608]):
            raise ValueError('legacy payload SHA1 mismatch')
        if sha(parts[0]) != KERNEL_SHA or sha(parts[3]) != DTB_SHA:
            raise ValueError('kernel/DTB not the pinned S7 payloads')
        if parts[0][56:60] != b'ARMd':
            raise ValueError('not a raw ARM64 kernel')
        return Boot(data[:PAGE], parts, offset + len(FOOTER))

    def native(self, ramdisk: bytes) -> bytes:
        if not ramdisk.startswith(b'\xfd7zXZ\x00'):
            raise ValueError('native ramdisk must use kernel-supported XZ')
        parts = self.parts.copy()
        parts[1] = ramdisk
        header = bytearray(self.header)
        struct.pack_into('<I', header, 16, len(ramdisk))
        header[576:596] = digest(parts)
        data = bytes(header)
        for p in parts:
            data += p + bytes(align(len(p)) - len(p))
        data += FOOTER
        if len(data) > CAPACITY:
            raise ValueError(f'native BOOT requires {len(data)} bytes, capacity is {CAPACITY}')
        data += bytes(CAPACITY - len(data))
        checked = Boot.parse(data)
        if checked.parts[0] != self.parts[0] or checked.parts[2:] != self.parts[2:]:
            raise ValueError('protected payload was changed')
        allow = set(range(16, 20)) | set(range(576, 596))
        if any(a != b and i not in allow for i, (a, b) in enumerate(zip(self.header, checked.header))):
            raise ValueError('protected header byte was changed')
        return data


def entry(name: str, data: bytes, mode: int, ino: int) -> bytes:
    # Names are generated by this tool, never taken as paths to extract from a ZIP.
    path = PurePosixPath(name)
    if name.startswith('/') or '..' in path.parts or '\x00' in name or str(path) != name:
        raise ValueError(f'unsafe cpio name: {name!r}')
    n = name.encode('ascii') + b'\x00'
    major,minor=0,0
    if stat.S_ISCHR(mode):
        nodes={'dev/console':(5,1), 'dev/null':(1,3)}
        if name not in nodes or data: raise ValueError('unapproved character device')
        major,minor=nodes[name]
    if stat.S_ISBLK(mode): raise ValueError('block device nodes forbidden in generated ramdisk')
    fields = [ino, mode, 0, 0, 2 if stat.S_ISDIR(mode) else 1, 0, len(data), 0, 0, major, minor, len(n), 0]
    header = b'070701' + b''.join(f'{v:08x}'.encode('ascii') for v in fields)
    return header + n + bytes((-len(header)-len(n)) % 4) + data + bytes((-len(data)) % 4)


def parse_cpio(data: bytes) -> dict[str, tuple[int, bytes]]:
    out, off = {}, 0
    while off + 110 <= len(data):
        if data[off:off+6] != b'070701':
            raise ValueError('not newc cpio')
        f = [int(data[off+6+i*8:off+14+i*8], 16) for i in range(13)]
        mode, size, ns = f[1], f[6], f[11]
        if not 1 <= ns <= 4096 or size > 32*1024*1024:
            raise ValueError('unreasonable cpio entry')
        name_bytes = data[off+110:off+110+ns]
        if len(name_bytes) != ns or not name_bytes.endswith(b'\x00'):
            raise ValueError('truncated cpio name')
        name = name_bytes[:-1].decode('ascii')
        start = align(off+110+ns, 4)
        end = start+size
        if end > len(data):
            raise ValueError('truncated cpio data')
        content = data[start:end]
        off = align(end, 4)
        if name == 'TRAILER!!!':
            if size or any(data[off:]):
                raise ValueError('invalid cpio trailer')
            return out
        if name in out or name.startswith('/') or '..' in PurePosixPath(name).parts:
            raise ValueError('duplicate/unsafe cpio entry')
        out[name] = mode, content
    raise ValueError('missing cpio trailer')


def check_arm64_init(b: bytes) -> None:
    if len(b) < 64 or b[:7] != b'\x7fELF\x02\x01\x01' or struct.unpack_from('<H', b, 18)[0] != 183:
        raise ValueError('init must be a Linux ARM64 little-endian ELF64 executable')
    off = struct.unpack_from('<Q', b, 32)[0]
    size, count = struct.unpack_from('<HH', b, 54)
    if size != 56 or count < 1 or off+size*count > len(b):
        raise ValueError('invalid ELF program headers')
    types = [struct.unpack_from('<I', b, off+i*size)[0] for i in range(count)]
    if 3 in types or 2 in types:  # PT_INTERP / PT_DYNAMIC
        raise ValueError('native init must not depend on a runtime linker/Android shared libraries')


def make_ramdisk(init_path: Path, fw_dir: Path, usb_trial_fps: int = 0, lab_seconds: int = 0,
                 direct_mfc_lab: bool = False, direct_mfc: bool = False,
                 gpu_probe: bool = False, ui_diagnostics: bool = False,
                 windows_package_pin: Path | None = None) -> tuple[bytes, list[dict]]:
    if usb_trial_fps not in (0, 120, 240):
        raise ValueError('only explicit rear 720p120/240 USB trials are supported')
    if lab_seconds not in (0, 120) or (lab_seconds and usb_trial_fps):
        raise ValueError('laboratory monitor BOOT must be an isolated 120-second profile')
    if direct_mfc_lab and (lab_seconds != 120 or usb_trial_fps):
        raise ValueError('direct MFC comparison requires an isolated lab120 profile')
    if direct_mfc and (lab_seconds or usb_trial_fps or direct_mfc_lab):
        raise ValueError('persistent direct MFC requires a normal unbounded profile')
    if gpu_probe and not direct_mfc:
        raise ValueError('GPU probe requires the normal direct MFC profile')
    use_direct_mfc = direct_mfc_lab or direct_mfc
    backend = 'mfc' if use_direct_mfc else 'mediacodec'
    init = init_path.read_bytes()
    check_arm64_init(init)
    entries: dict[str, tuple[int, bytes]] = {}
    def add(name: str, mode: int, content: bytes = b'') -> None:
        if name in entries:
            raise ValueError('duplicate generated root path')
        entries[name] = mode, content
    for d in ('dev','dev/input','proc','sys','run','tmp','config','sbin','lib','lib/firmware','etc','etc/s7-camera','system','system/vendor','system/etc','system/bin','opt','opt/s7-hub','opt/s7-hub/bin','opt/s7-hub/lib64'):
        add(d, stat.S_IFDIR | (0o1777 if d == 'tmp' else 0o755))
    add('dev/console', stat.S_IFCHR | 0o600)
    add('dev/null', stat.S_IFCHR | 0o666)
    add('init', stat.S_IFREG | 0o755, init)
    if windows_package_pin is not None:
        if windows_package_pin.is_symlink() or not windows_package_pin.is_file() or windows_package_pin.stat().st_size > 4096:
            raise ValueError('Windows package pin must be a small regular file')
        pin = json.loads(windows_package_pin.read_text(encoding='utf-8-sig'))
        if not {'media_bytes','media_sha256'} <= set(pin):
            raise ValueError('Windows package pin must include installer disk identity; use build_install_media output PHONE_PACKAGE.json')
        if (not {'schema','release','bytes','sha256','device_serial'} <= set(pin) or set(pin) - {'schema','release','bytes','sha256','device_serial','media_bytes','media_sha256'} or pin['schema'] != 'S7_PHONE_PACKAGE_1' or
                type(pin['release']) is not int or not 0 < pin['release'] <= 0xffffffff or
                type(pin['bytes']) is not int or not 0 < pin['bytes'] <= 128*1024*1024 or
                not isinstance(pin['device_serial'], str) or len(pin['device_serial']) != 18 or
                any(c not in '0123456789abcdef' for c in pin['device_serial']) or
                not isinstance(pin['sha256'], str) or len(pin['sha256']) != 64 or
                any(c not in '0123456789abcdef' for c in pin['sha256'])):
            raise ValueError('Invalid Windows package pin')
        if 'media_bytes' in pin or 'media_sha256' in pin:
            if (type(pin.get('media_bytes')) is not int or not 0 < pin['media_bytes'] <= 256*1024*1024 or
                    not isinstance(pin.get('media_sha256'), str) or len(pin['media_sha256']) != 64 or
                    any(c not in '0123456789abcdef' for c in pin['media_sha256'])):
                raise ValueError('Invalid Windows installer disk pin')
        add('etc/s7-windows-package.json', stat.S_IFREG | 0o444, (json.dumps(pin, sort_keys=True)+'\n').encode('ascii'))
    add('sbin/hotplug', stat.S_IFLNK | 0o777, b'/init')
    # Compatibility firmware lookup paths only. No Android SYSTEM mounted/started.
    add('system/vendor/firmware', stat.S_IFLNK | 0o777, b'/lib/firmware')
    add('system/etc/firmware', stat.S_IFLNK | 0o777, b'/lib/firmware')
    add('vendor', stat.S_IFLNK | 0o777, b'/system/vendor')
    from sensorhub_payload import load as load_sensorhub
    for name, (mode, data) in load_sensorhub(fw_dir.parent/'sensorhub').items():
        add(name, mode, data)

    from mediacodec_payload import load as load_mediacodec
    for name, (mode, data) in load_mediacodec(fw_dir.parent/'mediacodec', monitor=backend, camera=backend).items():
        add(name, mode, data)

    menu_dir = Path(__file__).resolve().parents[1]/'hardware/gpu'
    menu_pin = json.loads((menu_dir/'GPU_MENU_MANIFEST.json').read_text())
    menu_path = menu_dir/'menu-worker-arm64'
    if menu_pin.get('schema') != 'S7-GPU-MENU-1' or menu_pin.get('path') != menu_path.name or menu_path.is_symlink():
        raise ValueError('invalid GPU menu manifest/path')
    menu_blob = menu_path.read_bytes()
    menu_source = Path(__file__).resolve().parents[1]/'native/support/gpu'
    if menu_pin.get('source_sha256') != sha((menu_source/'menu_worker.c').read_bytes()) or menu_pin.get('kernel_sha256') != sha((menu_source/'menu.cl').read_bytes()):
        raise ValueError('GPU menu binary is stale relative to its sources')
    if len(menu_blob) != menu_pin.get('bytes') or len(menu_blob) > 256*1024 or sha(menu_blob) != menu_pin.get('sha256') or menu_blob[:7] != b'\x7fELF\x02\x01\x01' or struct.unpack_from('<H', menu_blob,18)[0] != 183:
        raise ValueError('GPU menu ELF/hash mismatch')
    add('opt/s7-gpu', stat.S_IFDIR | 0o755)
    add('opt/s7-gpu/bin', stat.S_IFDIR | 0o755)
    add('opt/s7-gpu/bin/menu-worker-arm64',stat.S_IFREG | 0o755,menu_blob)
    for directory in ('usr','usr/share','usr/share/licenses','usr/share/licenses/s7-menu'):
        if directory not in entries:
            add(directory,stat.S_IFDIR | 0o755)
    for name in ('IBMPlex-OFL.txt','DSEG-OFL.txt'):
        add('usr/share/licenses/s7-menu/'+name,stat.S_IFREG | 0o444,(fw_dir.parent/'fonts'/name).read_bytes())

    if gpu_probe:
        probe_dir = Path(__file__).resolve().parents[1]/'hardware/gpu'
        pin = json.loads((probe_dir/'GPU_PROBE_MANIFEST.json').read_text())
        probe_path = probe_dir/'gpu-probe-arm64'
        if pin.get('schema') != 'S7-GPU-PROBE-1' or pin.get('path') != probe_path.name or probe_path.is_symlink() or not probe_path.is_file():
            raise ValueError('invalid GPU probe manifest or path')
        probe = probe_path.read_bytes()
        if len(probe) != pin.get('bytes') or len(probe) > 65536 or sha(probe) != pin.get('sha256') or probe[:7] != b'\x7fELF\x02\x01\x01' or struct.unpack_from('<H', probe, 18)[0] != 183:
            raise ValueError('GPU probe ELF/hash mismatch')
        add('opt/s7-gpu/bin/gpu-probe-arm64', stat.S_IFREG | 0o755, probe)

    runtime_source = 'cache' if (lab_seconds or usb_trial_fps) else 'system'
    add('etc/s7-codec/runtime-source', stat.S_IFREG | 0o444,
        (runtime_source+'\n').encode('ascii'))

    if usb_trial_fps:
        add('etc/s7-codec/usb-trial-fps', stat.S_IFREG | 0o444, f'{usb_trial_fps}\n'.encode('ascii'))
    if lab_seconds:
        attempt = sha(init + (b'\0S7-NATIVE-LAB-120-MFC' if direct_mfc_lab else b'\0S7-NATIVE-LAB-120'))
        policy = {'schema':'S7-NATIVE-LAB-1', 'trial_seconds':lab_seconds,
                  'attempt_id':attempt, 'return_to_recovery':True}
        add('etc/s7-lab.json', stat.S_IFREG | 0o444,
            (json.dumps(policy, sort_keys=True, separators=(',',':'))+'\n').encode('ascii'))
    manifest, hashes = [], {}
    for path in sorted(p for p in fw_dir.iterdir() if p.suffix in {'.bin', '.wmfw'}):
        if path.is_symlink() or not path.is_file() or path.stat().st_size > 8*1024*1024:
            raise ValueError(f'unsafe firmware input {path.name}')
        b = path.read_bytes()
        h = sha(b)
        item = {'name': path.name, 'bytes': len(b), 'sha256': h, 'source': 'verified supplied SYSTEM/system/' + ('etc/firmware' if path.name.startswith('moon-dsp5-dsm.') else 'vendor/firmware')}
        name = f'lib/firmware/{path.name}'
        if h in hashes:
            add(name, stat.S_IFLNK | 0o777, hashes[h].encode('ascii'))
            item['deduplicated_to'] = hashes[h]
        else:
            add(name, stat.S_IFREG | 0o644, b)
            hashes[h] = path.name
        manifest.append(item)
    if not any(v['name'] == 'mfc_fw.bin' for v in manifest):
        raise ValueError('MFC firmware absent')
    status = {'name': 'S7 native Linux / revised native services', 'android_framework': False, 'pc_volume_consumer_hid_implemented': True, 'pc_volume_report_id': 9, 'pc_volume_hardware_accepted': False,
        'mediacodec_bridge_compiled': True, 'mediacodec_runtime_in_boot': False, 'monitor_codec_backend': 'mediacodec', 'camera_codec_backend': 'mediacodec', 'external_native_media_runtime': True, 'media_runtime_source': runtime_source, 'mediacodec_ipc_version': 3, 'mediacodec_shared_nv12_frames': True, 'mediacodec_pixel_slots_per_role': 1, 'mediacodec_raw_pixels_in_socket': False, 'mediacodec_separate_control_channel': True, 'mediacodec_initial_csd': True, 'mediacodec_binder_startup_gate': True,
        'camera_implemented': False,  # legacy whole-device readiness, not software mode coverage
              'camera_target_matrix_7_plus_3': True,
              'native_mfc_encoder_pinned_ext_controls': True,
              'native_mfc_encoder_generic_parm_ioctl_used': False,
              'native_mfc_encoder_separate_headers': True,
              'camera_cached_nv12_upload': True,
              'camera_padding_only_clear': True,
              'camera_compiled_transform_offsets': True,
              'camera_usb_retry_frames': 1, 'camera_usb_retry_max_bytes': 4*1024*1024,
              'camera_usb_retry_timeout_ms': 100,
              'camera_strict_transient_error_tree': True,
              'camera_encoder_per_pts_residence_guard': True,

              'camera_timestamp_ledger_and_rate_metrics': True,
              'native_otf_flite_pool_pump_implemented': True,
              'native_graph_owner_and_provider_wired': True,
              'native_camera_source_profile_count': 17, 'native_camera_hardware_approved_profile_count': 0,
              'camera_selection_hid_v2_implemented': True,
              'windows_front_rear_sources_code': True,
              'windows_front_rear_sources_build_verified': False,
              'audio_implemented': True, 'audio_default_enabled': False, 'hardware_accepted': False,
              'native_worker_registry_implemented': True, 'native_worker_limit': 32,
              'sensor_hub_cancellable_start_stop': True, 'sensor_hub_retryable_last_lease_cleanup': True, 'sensor_hub_strict_launch_integrity': True, 'sensor_iio_cleanup_retains_owner': True,
              'coexistence_harness_implemented': True, 'coexistence_hardware_accepted': False,
              'fixed_session_timeout_seconds': lab_seconds or None,
              'lab_automatic_usb': bool(lab_seconds),
              'operating_mode': 'one-shot-recovery-laboratory' if lab_seconds else 'thermal-and-liveness-supervised',
              'home_link_wave_implemented': True, 'home_link_driver_check': 'monitor protocol reply, not all PC drivers', 'home_link_hardware_tested': False,
              'native_encoder_implemented': True, 'native_camera_capture_start_path_implemented': True, 'native_camera_capture_hardware_accepted': False,
              'native_node_configuration_implemented': True,
              'native_prepared_graph_builder_implemented': True,
              'shared_camera_consumers_implemented': True, 'local_preview_implemented': True,
              'local_preview_usable_on_s7': False, 'local_preview_default_enabled': False,
              'native_uvc_producer_implemented': True, 'generic_nv12_capture_adapter': True,
              'herolte_sensor_isp_provider': 'native owner/provider with 17 source plans (9 normal 30fps, 4 normal 60fps, 4 diagnostic 120/240fps); public high FPS blocked; first sensor start unverified',
              'framebuffer_presenter': 'tiled CPU conversion and scanline-order cache; not zero-copy',
              'cache_writes': 'only after explicit native-menu consent',
              'device_rotation': 'manual 0/90/180/270; matching framebuffer/menu/touch',
              'autorotation': 'SSP reader/filter/worker with isolated vendor lhd startup; hardware unverified',
              'native_sensor_hub_startup_implemented': True, 'sensor_hub_runtime_executed': False, 'sensor_hub_uses_bionic': True, 'sensor_hub_android_services': [],
              'rear_torch_driver_implemented': True, 'rear_torch_default_enabled': False,
              'camera_control_ui_to_live_request_implemented': True, 'camera_control_hardware_accepted': False,
              'camera_digital_zoom_implemented': True, 'camera_metadata_layout_partial': True,
              'monitor_completed_frames_coalescing': True, 'monitor_phone_local_timing': True,
              'settings_schema': 10, 'selected_android_vendor_components': True, 'glass_snapshot_blur': True, 'glass_live_backdrop': False, 'glass_background_max_hz': 0, 'glass_pacing_adapts_to_compose_cost': False, 'monitor_presenter_worker': True, 'monitor_presenter_owned_nv12_frames': 2, 'monitor_presenter_dma_pointers_retained': False, 'direct_fb_userptr_scaler': True, 'antialiased_menu_text': True, 'indicator_scale_percent': [50,200,5], 'touchpad_polling_hz': [125,500,1000], 'touchpad_polling_default_hz': 125, 'revised_camera_matrix_7_plus_3_integrated': True, 'camera_all_target_capture_paths_complete': False, 'camera_high_fps_webcam_unlocked': False, 'camera_rear_60fps_source_profiles': True, 'camera_rear_240fps_source_profiles': True, 'camera_bounded_local_highfps_trial': True, 'camera_local_trial_includes_usb_or_encoder': False, 'brightness': 'normal panel range, separate manual/auto values, preferences opt-in',
              'auto_brightness_reader_implemented': True, 'auto_brightness_controller_implemented': True,
              'auto_brightness_full_implemented': False, 'auto_brightness_default_enabled': False,
              'auto_brightness': 'packed26 SSP lux, isolated lhd supervisor, shared mask lease, smooth limits/manual fallback; hardware unverified',
              'kernel_sha256': KERNEL_SHA, 'dtb_sha256': DTB_SHA, 'init_sha256': sha(init)}
    status.update(camera_usb_trial_fps=usb_trial_fps, camera_usb_trial_limit_seconds=30 if usb_trial_fps else 0,
                  camera_usb_trial_requires_home=True, camera_usb_trial_saves_preferences=False)
    status.update(monitor_codec_backend=backend, camera_codec_backend=backend,
                  external_native_media_runtime=not use_direct_mfc,
                  direct_mfc_lab=direct_mfc_lab, persistent_direct_mfc=direct_mfc)
    status.update(usb_automatic_start=not bool(usb_trial_fps), status_indicators='menu header only')
    status.update(ui_build_status(gpu_probe, ui_diagnostics))
    add('etc/s7-native.json', stat.S_IFREG | 0o444, (json.dumps(status, indent=2)+'\n').encode())
    camera_profiles = fw_dir.parent / 'source-config' / 'camera-profiles.json'
    profile_bytes = camera_profiles.read_bytes()
    profiles = json.loads(profile_bytes)
    from camera_profile_payload import validate_payload
    profiles = validate_payload(profile_bytes, fw_dir)
    add('etc/s7-camera/profiles.json', stat.S_IFREG | 0o444, profile_bytes)
    add('etc/firmware-manifest.json', stat.S_IFREG | 0o444, (json.dumps(manifest, indent=2)+'\n').encode())
    order = sorted(entries, key=lambda n:(n.count('/'), n))
    cpio = b''.join(entry(n, entries[n][1], entries[n][0], i+1) for i,n in enumerate(order))
    cpio += entry('TRAILER!!!', b'', 0, len(order)+1)
    cpio += bytes((-len(cpio)) % 512)
    if parse_cpio(cpio) != entries:
        raise ValueError('CPIO did not round trip')
    compressed = lzma.compress(cpio, format=lzma.FORMAT_XZ, check=lzma.CHECK_CRC32,
        filters=[{'id':lzma.FILTER_LZMA2, 'dict_size':4<<20, 'preset':9 | lzma.PRESET_EXTREME}])
    if lzma.decompress(compressed) != cpio:
        raise ValueError('XZ did not round trip')
    return compressed, manifest


def write_new(path: Path, b: bytes) -> None:
    if path.exists():
        raise FileExistsError(path)
    with path.open('xb') as out:
        out.write(b)
        out.flush()
        os.fsync(out.fileno())
    if sha(path.read_bytes()) != sha(b):
        raise OSError('readback mismatch')


def build(original: Path, init: Path, fw: Path, outdir: Path, usb_trial_fps: int = 0, lab_seconds: int = 0,
          direct_mfc_lab: bool = False, direct_mfc: bool = False, gpu_probe: bool = False, ui_diagnostics: bool = False,
          windows_package_pin: Path | None = None) -> dict:
    if not stat.S_ISREG(original.stat().st_mode):
        raise ValueError('input must be a regular file, never a device node')
    base_data = original.read_bytes()
    base = Boot.parse(base_data, pinned=True)
    ramdisk, manifest = make_ramdisk(init, fw, usb_trial_fps, lab_seconds, direct_mfc_lab, direct_mfc, gpu_probe, ui_diagnostics, windows_package_pin)
    image = base.native(ramdisk)
    result = Boot.parse(image)
    outdir.mkdir(parents=True, exist_ok=False)
    write_new(outdir/'BOOT_NATIVE_EXPERIMENTAL.img', image)
    write_new(outdir/'BOOT_INPUT_ROLLBACK.img', base_data)
    write_new(outdir/'native-ramdisk.cpio.xz', ramdisk)
    files = parse_cpio(lzma.decompress(ramdisk))
    report = {'schema':'S7_NATIVE_BOOT_1', 'source_release':'native-appliance-integration-20260920', 'camera_usb_trial_fps':usb_trial_fps, 'camera_usb_trial_limit_seconds':30 if usb_trial_fps else 0,
              'lab_trial_seconds':lab_seconds, 'lab_return_to_recovery':bool(lab_seconds), 'lab_automatic_usb':bool(lab_seconds),
              'input_sha256':sha(base_data), 'output_sha256':sha(image),
              'gpu_probe_in_boot':gpu_probe, 'gpu_menu_gpu_only':False,
              'boot_bytes':len(image), 'payload_end':result.payload_end, 'headroom_bytes':CAPACITY-result.payload_end,
              'kernel_sha256':sha(result.parts[0]), 'dtb_sha256':sha(result.parts[3]),
              'kernel_and_dtb_unchanged':True, 'header_only_ramdisk_size_and_sha1_changed':True,
              'input_vendor_signature_tail_not_reused':True,
              'header_cmdline_unchanged':True, 'ramdisk_bytes':len(ramdisk),
              'native_init_sha256':sha(files['init'][1]), 'ramdisk_entries':len(files),
              'ELF_ARM64_static':True, 'android_init_ART_APK_in_native_root':False,
              'firmware_manifest':manifest, 'mediacodec_bridge_compiled':True, 'mediacodec_runtime_in_boot':False, 'monitor_codec_backend':'mediacodec', 'camera_codec_backend':'mediacodec', 'external_native_media_runtime':True, 'media_runtime_source':'cache' if (lab_seconds or usb_trial_fps) else 'system', 'hardware_boot_tested':False, 'flashed':False,
              'production_ready':False, 'native_camera':'shared sensor consumers, local PiP, encode/UVC pipeline; native provider wired with 17 source profiles (four high-speed profiles restricted to local NV12 trials or explicit bounded engineering USB BOOT); no hardware frames measured', 'native_audio':'native ALSA implemented; defaults off; hardware not accepted',
              'native_encoder':'selected AMediaCodec Exynos AVC; native source, shared NV12, access-unit assembly and bounded USB retry; hardware unaccepted',
              'camera_cached_nv12_upload': True,
              'camera_padding_only_clear': True,
              'camera_compiled_transform_offsets': True,
              'camera_usb_retry_frames': 1, 'camera_usb_retry_max_bytes': 4*1024*1024,
              'camera_usb_retry_timeout_ms': 100,
              'camera_strict_transient_error_tree': True,
              'camera_encoder_per_pts_residence_guard': True,

              'fixed_session_timeout_seconds':lab_seconds or None, 'home_link_wave_implemented':True, 'home_link_driver_check':'monitor protocol reply, not all PC drivers', 'home_link_hardware_tested':False, 'mandatory_thermal_and_watchdog':True,
              'cache_policy':'explicit opt-in only; no formatting or journal replay',
              'manual_device_rotation_implemented':True, 'native_sensor_hub_startup_implemented':True, 'sensor_hub_runtime_executed':False, 'sensor_hub_uses_bionic':True, 'sensor_hub_android_services':[],
              'pc_volume_consumer_hid_implemented':True, 'pc_volume_report_id':9, 'pc_volume_hardware_accepted':False,
              'settings_schema':10, 'glass_style':'CPU glass; GPU-only replacement pending', 'glass_live_backdrop':True, 'glass_background_max_hz':0, 'glass_pacing_adapts_to_compose_cost':True, 'glass_pending_nv12_frames':1, 'monitor_presenter_worker':True, 'monitor_presenter_owned_nv12_frames':2, 'monitor_presenter_dma_pointers_retained':False, 'direct_fb_userptr_scaler':True, 'indicator_scale_percent':[50,200,5], 'touchpad_polling_hz':[90], 'touchpad_polling_default_hz':90, 'revised_camera_matrix_7_plus_3_integrated':True, 'camera_all_target_capture_paths_complete':False, 'camera_high_fps_webcam_unlocked':False, 'camera_rear_60fps_source_profiles':True, 'camera_rear_240fps_source_profiles':True, 'camera_bounded_local_highfps_trial':True, 'camera_local_trial_includes_usb_or_encoder':False, 'device_brightness_and_power_menu_implemented':True,
              'native_worker_registry_implemented':True, 'native_worker_limit':32,
              'sensor_hub_cancellable_start_stop':True, 'sensor_hub_retryable_last_lease_cleanup':True, 'sensor_hub_strict_launch_integrity':True, 'sensor_iio_cleanup_retains_owner':True,
              'coexistence_harness_implemented':True, 'coexistence_hardware_accepted':False,
              'rear_torch_driver_implemented':True, 'rear_torch_default_enabled':False,
              'camera_control_ui_to_live_request_implemented':True, 'camera_control_hardware_accepted':False,
              'camera_digital_zoom_implemented':True, 'camera_metadata_layout_partial':True,
              'monitor_completed_frames_coalescing':True, 'monitor_phone_local_timing':True,
              'auto_brightness_reader_implemented':True, 'auto_brightness_controller_implemented':True,
              'auto_brightness_full_implemented':False, 'auto_brightness_default_enabled':False}
    use_direct_mfc = direct_mfc_lab or direct_mfc
    report.update(monitor_codec_backend='mfc' if use_direct_mfc else 'mediacodec',
                  camera_codec_backend='mfc' if use_direct_mfc else 'mediacodec',
                  external_native_media_runtime=not use_direct_mfc,
                  direct_mfc_lab=direct_mfc_lab, persistent_direct_mfc=direct_mfc)
    report.update(usb_automatic_start=not bool(usb_trial_fps), status_indicators='menu header only',
                  native_encoder='direct hardware MFC AVC; hardware acceptance pending' if use_direct_mfc else report['native_encoder'])
    report.update(ui_build_status(gpu_probe, ui_diagnostics))
    report['windows_package_pin'] = json.loads(files['etc/s7-windows-package.json'][1]) if windows_package_pin else None
    report['windows_package_delivery_hardware_tested'] = False
    write_new(outdir/'BOOT_BUILD.json', (json.dumps(report, indent=2)+'\n').encode())
    write_new(outdir/'RAMDISK_CONTENTS.txt', ''.join(f'{m:07o} {len(b):9d} {n}\n' for n,(m,b) in sorted(files.items())).encode())
    if sha(original.read_bytes()) != INPUT_SHA:
        raise OSError('source changed during build')
    return report


def main() -> None:
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--input-boot',type=Path,required=True)
    ap.add_argument('--init',type=Path,required=True)
    ap.add_argument('--firmware-dir',type=Path,required=True)
    ap.add_argument('--output-dir',type=Path,required=True)
    ap.add_argument('--windows-package-pin',type=Path,
                    help='pin the signed Windows package stored read-only on SYSTEM; not an automatic clean-PC bootstrap')
    ap.add_argument('--highfps-usb-trial', type=int, choices=(120,240), default=0,
                    help='explicit engineering BOOT: fixed rear mode, Home required, 30s capture limit, no preference writes')
    ap.add_argument('--lab-seconds', type=int, choices=(120,), default=0,
                    help='one-shot monitor/input laboratory BOOT; CACHE marker and Recovery exit are mandatory')
    ap.add_argument('--direct-mfc-lab', action='store_true',
                    help='explicit lab120-only direct hardware V4L2 comparison; do not start native media services')
    ap.add_argument('--direct-mfc', action='store_true',
                    help='persistent direct hardware V4L2 mode; no laboratory timer or automatic Recovery return')
    ap.add_argument('--gpu-probe', action='store_true',
                    help='include only the pinned GPU diagnostic, not Mali libraries or a GPU menu')
    ap.add_argument('--ui-diagnostics', action='store_true',
                    help='engineering only: on-demand scanout capture and bounded local-menu control over USB; no shell')
    args=ap.parse_args()
    result=build(args.input_boot,args.init,args.firmware_dir,args.output_dir,args.highfps_usb_trial,
                 args.lab_seconds,args.direct_mfc_lab,args.direct_mfc,args.gpu_probe,args.ui_diagnostics,
                 args.windows_package_pin)
    print(json.dumps({k:v for k,v in result.items() if k!='firmware_manifest'},indent=2))
if __name__=='__main__':
    main()
