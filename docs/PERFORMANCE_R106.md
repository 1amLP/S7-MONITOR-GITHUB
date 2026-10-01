# Monitor r104-r108: Changes and Measurements

30 September 2026. One SM-G930F / Exynos 8890, Windows 11 x64, NVIDIA RTX 3060.
Historical continuation of [r103](AUDIT_R103.md), not current release acceptance.

## Installed State

- Windows Monitor 0.3.30.0, signed package 2026093005.
- Kernel `3.18.140-g3dfe42cdf48c`, retaining the DWC3 fix.
- BOOT, Windows archive and installer image written to the phone with hash
  readback. Windows installed from the phone without a PC restart; package match
  was confirmed.
- Camera and manager/Sniper binaries retained from the accepted version.
- Shared USB deadlines, thermal/watchdog and 50/80 charging were not weakened.

## Changes

Monitor now gives the hardware H.264 MFT retained D3D11 NV12 surfaces. Per-frame
NV12 staging readback and copying into a new MF buffer were removed. The pool
is bounded to six samples; a surface is not reused while the MFT or queue owns
it. Static-image refresh gets new timestamps while retaining pixel ownership.

The old memory path remains for incompatible GPU/MFT combinations. Sniper still
uses readback/shared memory. Monitor's zero-copy claim does not apply to Sniper
or virtual-camera output.

Active acquisition retry after a missing notification was shortened from 50 to
16 ms, consistent with the wait cycle in the
[Microsoft IddCx sample](https://github.com/microsoft/Windows-driver-samples/blob/main/video/IndirectDisplay/IddSampleDriver/Driver.cpp).
Counters distinguish notifications, timeouts, frames acquired after timeouts,
GPU-conversion calls and FinishedProcessingFrame duration. Windows GPU settings
were not changed. A shorter timeout alone did not establish the cause of all drops.

The phone decoder wakes on compressed frames, generation changes and returned
DMA buffers. Pending work is checked after 2 ms; no work after 250 ms. The first
IDR survives a generation change. Dependent compressed H.264 AUs are never
coalesced into a newer compressed frame.

MFC now synchronizes the occupied compressed OUTPUT range (`bytesused`) instead
of the entire 1 MiB allocation, using the old kernel's existing exact-range
helpers. RAW/NV12 CAPTURE, DRM and the camera encoder are unchanged. Changing
ION attributes for all NV12 was rejected because it did not match this kernel's
actual path.

The installer channel also had a race: the host could issue another request
after receiving a response but before phone write completion. The busy flag
discarded that request. A bounded next-command slot now waits for completion.
Two full r105 package downloads completed in 187/163 ms with matching SHA-256;
r106 automatic delivery succeeded too.

## Measurements

Monitor was 1280x720 at 60 Hz, ECO on, menu closed. Resolution and ECO were not
changed between these windows. PC monotonic time was used because phone and PC
UTC clocks differed. Counter snapshots are not atomic with that timer, so rates
are approximate.

| Scenario | Window, s | Received/s | Submitted/s |
| --- | ---: | ---: | ---: |
| r103, owner's video | 60.75 | 55.52 | 55.39 |
| r105, visible synthetic motion | 20.28 | 59.82 | 59.48 |
| r105, second short window | 6.24 | 51.42 | 51.42 |
| r105, motion plus front 720p30 | 6.22 | 59.85 | 59.37 |
| r106, visible motion, run 1 | 20.19 | about 60 | 59.59 |
| r106, visible motion, run 2 | 20.15 | about 60 | 59.91 |
| r106, owner's video, window 1 | 20.16 | 50.15 | 50.15 |
| r106, owner's video, window 2 | 20.27 | 56.55 | 56.55 |
| r106, owner's video, window 3 | 20.14 | 46.02 | 45.97 |
| r106, same video windowed | 30.22 | 44.00 | 44.04 |

The r106 source checked its position on S7, visibility and lack of occlusion.
It submitted about 75 Present/s, an API rate rather than 75 phone screen updates.
The driver acquired about 300 new frames per five seconds with no missing or
repeated frame numbers in those windows. CPU NV12 readbacks and allocator waits
were zero after startup.

GPU-conversion calls averaged 0.14-0.18 ms of CPU submission time, not GPU
timestamp duration. Neither r106 run added decoder restarts. One decoder timeout
during driver replacement recovered automatically and is not omitted.

Both r106 runs showed SoC 40 C and CPU snapshots of 15-22%, without thermal pause.
This is not an equal-duration thermal-equilibrium comparison or a measured
temperature reduction by a stated number of degrees.

Ordinary video still reproduced the drops at 40 C without new retries or USB
loss. Windows supplied only 223-252 new frames per five seconds; conversion calls
took about 0.15 ms and phone reception/output nearly matched. The 16 ms wait
change therefore did not fix normal playback.

YouTube statistics showed 2560x1440@60, one dropped frame out of 11730 and a
24.25-second buffer. They did not report major player-decoder loss. Active display
rates were 120, 75, approximately 60 and exactly 60 Hz for S7. Mixed-refresh timing
remained a hypothesis; display rates, DPI, registry and browser settings were not
changed. Leaving fullscreen still produced about 44 FPS on every measured stage.

Front 720p30 alongside Monitor in r105 produced 297 frames in 10 seconds,
29.77 FPS, with three source gaps and zero USB backpressure/retries. Capture
stopped after the test. The strict zero-gap check failed. No video was saved.

## Checks and Boundaries

- Private Windows x64 `/W4 /WX` builds and protocol, queue, I/O lifetime, NV12,
  camera and Sniper tests passed.
- Real-GPU tests passed at 720p/1440p for BT.709, H.264 output, surface ownership
  and retained-image IDR refresh.
- Targeted Go race tests for decoder wakeups and package delivery passed.
  Known full-suite failures were not fixed by these changes.
- The kernel built with earlier section-mismatch/FIPS warnings. The patch was
  checked by reverse application to the pinned tree.
- Sanitized Windows x64 and Linux ARM64 builds used a synthetic serial; they
  were not signed or flashed.
- The owner confirmed normal imagery after r105. Optical refresh and total
  host-to-panel latency were not measured.
- The owner reported a successful r103 WhatsApp call; 7019 captured frames reached
  USB queuing with zero source gaps/backpressure. This was not a multi-hour r106 call.
- The latest 1440p/full-load path and a cold, completely static desktop startup
  through the GPU-input MFT were not accepted.

## Additional r106-r108 Experiments

A 75 Present/s source repeating each image twice still produced about 60 received
FPS. Automatic identical-image suppression by Windows was not established on
that path. Limiting the same source to 60 Present/s reproduced the problem
without YouTube: 59.77 source Present/s and 46.35 received FPS. The old 75 Present/s
test was therefore insufficient for a 60 FPS source. Other display rates were
not changed, as requested by the owner.

Official, signature/hash-verified PresentMon 2.6.0 ran in two bounded sessions.
It did not expose the complete Chrome video stream. These incomplete observations
could not establish a browser-decoder cause. No input, images or video were
recorded, no PresentMon service was installed, and both sessions ended.

r107 requested a low-power hardware-H.264 GPU only for S7 through
IddCxAdapterSetRenderAdapter. Intel UHD 770 first passed synthetic 720p/1440p
BT.709 tests with 121 frames and three IDRs. Windows selected Intel for S7,
leaving other displays unchanged. Fixed60 reception still measured only
44.72 FPS. The GPU-selection hypothesis was rejected.

r108 / Monitor 0.3.30 restored normal Windows GPU selection. Driver.cpp matches
r106; Intel preference is not active. The version increased for a normal upgrade
from 0.3.29. BOOT/archive/media readbacks and package 2026093005 verification
passed; logs again showed RTX 3060. No PC reboot or Camera/manager change was needed.

A separate 20-second timing-only WGC CreateForMonitor test for S7 received
940 callbacks, about 46.99 FPS by arrival time and PTS, with monotonic timestamps.
In a nested 15-second window, IDD received 49.07 and submitted 49.01 FPS. Different
window lengths prevent an exact frame comparison. WGC also did not reach 60.
Pixels were not read back to CPU, uniqueness was not checked, and no video was
saved. The process exited; WGC was not added to the installed package.

Conclusion at r108: memory and wakeup improvements were installed, but the
60 FPS source's passage through Windows composition/acquisition remained open.
Neither universal FPS recovery nor a guaranteed heat reduction was established.
