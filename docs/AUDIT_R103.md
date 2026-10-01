# Historical Audit: r103

30 September 2026. **Not fully optimized and not production-ready.**
This report records r103, not the current implementation. Later Monitor memory
and presentation changes are documented in the performance reports.

Reviewed baseline: source `22c5800`, engineering firmware tree `c60d08f`, kernel
`3.18.140-gfd26b7e36c45`, Windows Monitor 0.3.26.0. The audit did not replace
the installed build or change driver settings.

## Scope

Reviewed the main Monitor capture/transport/decode path; FIMC/ISP/MFC, Preview,
WinUSB and Media Foundation camera paths; Sniper capture and HID; menus, input,
haptics and sensors; ALSA/UAC2; charging, thermal/watchdog; resource ownership,
diagnostics and signed-package delivery. The DWC3 patch was inspected.

This is not proof that every Linux line, Samsung/Mali binary or Windows client
is correct. Closed vendor internals and every possible application were outside
the review's reach.

## Confirmed Findings

### P2: Control operations can be rejected

`UI.async` uses a nonblocking queue with one pending slot. If its worker is busy
and another command is already queued, the next operation is rejected with
`control operation already pending`. The live audit snapshot contained that error.
Camera sliders use the same queue, so their final desired value is not guaranteed
to be the last applied value under contention. Coalescing by parameter and
separate one-shot command handling are needed; a larger FIFO can merely preserve
more stale actions.

Code: [control queue](../native/internal/appliance/run_linux.go#L1101),
[camera actions](../native/internal/appliance/function_menu.go#L328).

### P2: Invalid saved camera controls have no menu reset

A saved exposure/focus/white-balance selection can pass the generic settings
envelope but fail the active sensor's limits. `ControlState` returns an error;
the menu then exposes only Back, and capture refuses those values. There was no
menu command to reset that bank to defaults. This describes a recovery edge case,
not a claim that the connected camera was currently broken.

The existing regression creates this state but then panics on an obsolete menu
row index. Source inspection independently confirms the missing recovery path.

Code: [control menu](../native/internal/appliance/camera_3a_linux.go#L87),
[sensor validation](../native/internal/camera/shared.go#L211).

### P2: Menu rendering owns the presentation lock

`menuPaintWorker` holds `presentationMu` while preparing and submitting a scene.
Monitor uses the same lock. The audit observed 344 renders averaging 23.59 ms,
maximum 43.10 ms, including waits rather than just OpenCL time. These exceed a
16.67 ms frame budget. A separate worker alone does not remove that contention.
Preparation outside the short publication section needs preserved DMA ownership
and generation checks. The exact contribution to each missing frame was not measured.

Code: [menu worker](../native/internal/appliance/menu_render_linux.go#L58).

## Performance Opportunities at r103

| Path | Cost then present | Implication |
| --- | --- | --- |
| PC Monitor | GPU NV12 to staging, CPU vector and new MF buffer | Two full CPU copies and readback per frame; later changed in r104-r106 |
| PC Camera | New output MF buffer and decoded NV12 copy | A pool or compatible GPU samples may help, with stride/crop/ownership preserved |
| MJPEG | WIC JPEG encoding on CPU | A compatibility mode with its own cost |
| Sniper | Readback, shared-memory copy, driver copy and MF buffer | Additional full-frame transfers across processes |
| Windows USB | New operation, event and buffer per 16 KiB chunk | A pool requires proven I/O completion before reuse |
| S7 Monitor | Decoder polling every 5 ms | 200 wakeups/s and up to 5 ms added phase; later changed |
| FunctionFS | Sleep(1 ms) retries on EAGAIN/zero reads | Extra wakeups; changes must respect old-kernel cancellation semantics |
| Compressed S7 frames | New payload for each H.264 AU | Pooling must preserve ownership through submission or discard |

One 2560x1440 NV12 image occupies 5,529,600 bytes. At 60 FPS, one full copy moves
about 332 MB/s of useful data. That arithmetic is not a measured bus load or proof
of an FPS bottleneck. A WIC test encoded a 2560x1440 JPEG in about 6.48 ms on its
specific content, not a guaranteed live-stream rate.

Relevant code: [desktop readback](../host-windows/monitor/DesktopReadback.cpp),
[encoder](../host-windows/monitor/Encoder.cpp),
[camera output](../host-windows/camera/Source.cpp),
[Sniper](../host-windows/camera/Sniper.cpp),
[native pipeline](../native/internal/appliance/run_linux.go),
[USB receive](../native/internal/appliance/usb_linux.go),
[H.264 payloads](../native/pkg/monitor/protocol.go).

## Live Observation

Over 428.00 seconds of one Monitor generation: received 55.65 FPS, decoded
55.64 FPS, submitted 54.80 FPS. This was ordinary use with menu interaction and
some camera activity, not a fixed 60 FPS source. Most of the missing-to-60 frames
were already absent before S7 decoding, but that alone did not locate the PC cause.

- Monitor used MFC NV12 DMA-BUF -> VPP -> DECON with zero CPU copied bytes.
- Windows capture/encoder summaries had similar cadence, with frame-ID gaps
  visible before encoding. Later reports explain why such gaps are not always
  lost images.
- The front-camera session captured, encoded and queued 2085 frames; source
  gaps and USB backpressure were zero. Capture stopped when the client closed.
- SoC snapshots were 45 and 40 C; no thermal pause occurred. This is not the
  maximum temperature for all workloads.
- Native init RSS went from about 17.3 to 16.3 MiB. Open descriptors returned
  from 133 to 69 after camera closure. No growing leak was observed in this short
  window; multi-day stability was not established.
- Input processing averaged about 0.054 ms, but one read-to-UI interval reached
  261.69 ms.
- Audio used 10 ms blocks and a 1920-sample target at 48 kHz, or 40 ms. A smaller
  queue requires combined-load XRUN testing.

Earlier camera results are retained in [Status](STATUS.md): about 59 FPS at
1080p60, about 119 FPS at 720p120; strict 240 FPS acceptance failed.

## Tests

Windows x64 Monitor, Camera, manager and FPS probe rebuilt with `/W4 /WX`.
INF/catalog checks, protocol/framing, queues, I/O ownership, NV12, shader,
Sniper and camera contracts passed. The unsigned candidate was not installed.
Targeted Go race tests for camera, graph, DECON, media, lifecycle, Monitor,
FunctionFS and selected appliance flows also passed.

Full `go test ./...` failed in six packages: appliance, audio, fimg2d, selinux,
uvcout and hid. A selected green subset must not be called a full PASS.

- Audio tests expected the old 960-sample target and a 1600 upper bound; current
  target was 1920. This is a stale criterion, not proof of live regulator failure.
- G2D expected fourfold downscaling while code used eightfold.
- HID sent all feature reports into DigitizerControl although camera/Sniper
  extensions were handled by the outer transport handler.
- High-FPS UVC tests exercised the earlier path rather than current WinUSB.
- The old MediaCodec SELinux label plan disagreed with its validator. Direct MFC
  was active; the alternative runtime was not accepted.
- Appliance tests retained stale row indices. The saved-3A recovery test panicked.
  A separate attempt hung in `TestBrightnessMenuPersistsOnlySuccessfulWrites`
  waiting for a command after selecting an outdated row. Only that test process
  was stopped and its stack was retained in the engineering audit.

## Existing Strengths and Next Gates

Hardware H.264, direct DMA Monitor output, bounded queues, ordered dependent
H.264 frames, shared camera/Preview capture, HAL 3.2 ISP setup, separate input
and rendering workers, real Sniper HID, confirmed DMA/I/O retirement, package
signatures, atomic settings persistence, thermal/watchdog and 50/80 charging
are valuable safeguards. Disabling them or inflating timeouts is not optimization.

Priorities were: restore current menu/WinUSB regression tests; fix command loss
and invalid-control recovery; shorten shared presentation ownership; validate
GPU sample reuse; compare event-driven decoding; and test long calls plus PC
startup after overnight idle. The complete former WhatsApp USB loss was not
reproduced in this audit. See [Diagnostics](DIAGNOSTICS.md).
