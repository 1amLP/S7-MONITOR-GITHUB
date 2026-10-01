# Menu Fixes: r116

1 October 2026. Follow-up to the [r115 release audit](AUDIT_R115.md).
The owner explicitly requested the menu fixes as well as English documentation.
Firmware labels were already English; this was not a language-only reflash.

## Changes

- One existing control worker still executes hardware operations. A bounded
  queue reserves separate capacity for one-shot commands and named settings.
  Pending values for the same setting coalesce to the latest request; unrelated
  commands keep their order. The input loop does not wait for hardware I/O.
- A slider can return to its currently displayed value while an earlier write
  is pending. The same intent is preserved during a concurrent PC camera write.
- Camera edits operate on fresh shared controls, preserving unrelated changes
  made by a Windows client. Changing a camera mode rejects stale queued actions.
- Manual brightness retains priority over an older pending automatic-mode request.
- The right pane cannot reinterpret an old button or slider after its meaning,
  range or page changes but before the new scene is painted. Normal value updates
  do not cancel the gesture. Preview option dispatch also checks the painted option.
- Camera control pages now offer **RESET CAMERA CONTROLS**, with the explicit
  **RESET FOR THIS MODE** action. It restores the current sensor/video-mode
  control bank even if saved values no longer fit that sensor's limits.

Reset does not change resolution, FPS, bitrate, image zoom or USB configuration.
It does not start capture or restart Monitor. It affects focus/exposure/color
controls for the selected camera mode, not every bank on both cameras.

## Verification

Three new regression tests failed before the fix: a busy worker lost final
brightness, a return-to-zero camera gesture became eight, and the invalid bank
had no Reset action. They passed after the changes.

Three targeted race runs covered queue bounds, reserved setting capacity,
coalescing/order, updates during execution, cancellation, PC/phone control sharing,
default recovery, stale sensor/page handling, manual brightness, haptics, Preview,
power and rotation. Tests verify that reset opens no sensor and changes no USB or
camera capture generation. Stale camera/brightness fixtures were updated to use
the actual current menu actions rather than obsolete row numbers.

Private and sanitized ARM64 builds passed. r116 BOOT was written with SHA-256
readback. The unchanged Windows archive and installer disk were verified on the
phone. Package 2026093008 matched after boot. Monitor frames resumed and no current
runtime error was reported.

The owner confirmed that brightness keeps the selected value and that the English
Reset entry is visible. No invalid preferences were injected into the real phone;
that recovery case was tested with the real control-bank code and a fake sensor.

After that interaction, diagnostics showed actual and saved brightness both at
70%, 199 coalesced setting updates, no pending operations and no rejected commands.
Monitor remained connected, with no current runtime error. Synthetic frame and
high-FPS camera probes were inactive. These are bounded observations, not a
long-duration acceptance test.

## Remaining Limits

The final full native suite (`go test -json -count=1 -timeout=90s ./...`) completed
with 29 passing packages, 6 failing packages and 8 without tests. Counting test
and subtest events, it reported 1,409 passes, 73 failures and 42 skips. The failing
packages remain `appliance`, `audio`, `fimg2d`, `selinux`, `uvcout` and `pkg/hid`.
The broader run exposed migration expectations, obsolete menu routes and CPU/UVC
fixtures incompatible with the current DMA/WinUSB path. Individual failures
still need classification and correction; not every failure is proven to be a
stale test. Passing the targeted menu tests does not certify all features.

Windows drivers, camera transport, display timing, kernel and package bytes were
not changed. The [r115 FPS measurements](PERFORMANCE_R115.md) remain historical
measurements of that presentation path, not a new 1440p or full-load r116 test.
Compiler maintenance, signing/install acceptance and long hardware tests remain
production blockers.
