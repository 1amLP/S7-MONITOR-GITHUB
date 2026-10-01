# Current Status

**r116 is the current device build, not a production release.**
It retains the r115 presentation path and adds [verified menu fixes](MENU_R116.md).
The [current audit](AUDIT_R115.md) separates file hygiene, build success and
hardware acceptance. Six native test packages still fail. Targeted passing tests
must not be described as a complete regression pass.

## Confirmed on One SM-G930F

- Monitor, front/rear cameras, zoom, Speaker/Microphone and Sniper hardware HID
  Touch have been used on a real phone and Windows PC.
- Sniper Touch continued working after focusing Task Manager on the tested PC.
- Earlier ARM64 DECON/MFC frame-ownership tests passed in TWRP.
- The identified DWC3 USB-wakeup panic had `PC=0` from a missing `resume`
  callback. The guarded callback kernel booted with verified BOOT readback.
- Current kernel `3.18.140-g3dfe42cdf48c` retains that fix and the compressed-input
  MFC cache optimization. DTB was not changed.
- Camera HAL 3.2 setup improved the tested rear 1080p60 path from 33.33 to
  59.13 FPS in a ten-second check. A 30-second Monitor-plus-camera test measured
  58.99 FPS without a USB error. Front 1440p30 delivered 300 frames in ten seconds.
- Rear 720p120 measured 119.10 FPS without calculated gaps. 720p240 measured
  238.03 FPS but failed strict acceptance with 28 calculated gaps.
- The owner reported much less camera delay. Sensor-to-screen latency has not
  been measured end to end. Local Preview has a separate 30 FPS limit.
- Windows Monitor 0.3.33.0/package 2026093008 is installed and stored on the
  phone. Package matching was confirmed again during the latest audit.
- r113 test tags measured 59.94-60.01 unique decoded FPS and 59.65-59.70 unique
  presentation requests/s, without repeated or invalid tags in those windows.
- r115 combines menu telemetry with the next video frame. One controlled
  two-minute source measured 59.650 unique output FPS without the menu and
  59.528 with it. Twenty-four menu rebuilds added no old-video submissions.
  Small losses remain. TE delivered about 59.61 interrupts/s, not an optical
  proof of exactly 60 physical frames.
- Monitor uses GPU NV12 surfaces on the PC and direct MFC NV12 DMA-BUF/VPP/DECON
  on the phone. Sniper and camera output still have separate copy costs.
- Sanitized Windows x64 and Linux ARM64 source builds pass. The fresh sanitized
  candidate was not signed, installed or flashed.
- Firmware menus and component source strings are English. Windows application
  dialogs use the operating system's language.
- r116 preserves final queued menu settings and adds a current-mode camera-control
  reset. The owner confirmed stable brightness selection and the English Reset
  entry. Unit/race tests cover invalid saved banks without opening a sensor or
  changing USB; the working phone was not given intentionally invalid settings.

## Not Yet Accepted

- A maintained Go toolchain and documented ongoing security maintenance for
  the vendor Linux 3.18 kernel. Reproducing the old binary is not that validation.
- Full native regression suite: six failing packages in the audit baseline,
  including stale checks. The camera-control recovery path was fixed in r116,
  but the wider suite still fails. The source publication excludes the
  engineering test fixtures; `-SkipTests` does not make the release tested.
- Complete signing/install/update/rollback workflow of the serial-parameterized
  sanitized source on another PC or handset.
- Long 2560x1440 and full simultaneous-load stability after the latest changes.
- Lossless end-to-end 240 FPS and 120/240 FPS availability in Windows Camera.
- Cold PC startup after overnight idle and repeated USB suspend/resume after the
  kernel fix. A known panic cause was fixed; absence of all reboots is not proven.
- Long-term 50-80% charge-policy behavior, thermal equilibrium and other phones.
- Every client application's camera-mode and control behavior.

The camera uses WinUSB bulk, not isochronous UVC. UAC2 audio uses isochronous
endpoints. Starting those streams does not rebuild the whole composite USB gadget;
exposing the setup disk is a separate explicit operation.

Earlier combined Monitor/camera use lost the link with `camera pipeline stalled`,
heartbeat loss and Windows camera error `0x80070079`. The specific 75 ms Monitor
deadline was not confirmed as that cause. Bounded fault diagnostics now retain
the relevant state. The owner reported a successful WhatsApp call after r103,
but this is not multi-hour acceptance of the current build.

## Historical Evidence

- [r103 audit](AUDIT_R103.md): original limitations and regression failures.
- [r104-r108](PERFORMANCE_R106.md): memory/wakeup work and rejected GPU-selection experiment.
- [r109-r111](PERFORMANCE_R110.md): precise acquisition retry and decoded-frame ordering.
- [r112-r113](PERFORMANCE_R112.md): virtual 120 Hz capture with 60 FPS transport and pixel counters.
- [r114-r115](PERFORMANCE_R115.md): menu cadence and delivered display interrupts.

Earlier 40-50 FPS drops were absent from the successful controlled r112 runs,
not disproved for every source. Earlier ordinary-video and fixed60 failures are
kept in the historical reports rather than erased. For a future loss, follow
[Diagnostics](DIAGNOSTICS.md) before restarting the device.

Private source publication is reasonable with these limits stated. A
production-ready firmware or installer claim is not supported by the evidence.
