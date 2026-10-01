# Monitor FPS with the Menu Open

1 October 2026. Samsung Galaxy S7 SM-G930F, 1280x720.
Windows Monitor 0.3.33.0 and package 2026093008 were unchanged.

## Cause and Changes

Before the change, the phone received and decoded about 60 FPS but submitted
57.64 FPS with the menu open. Over 15.23 seconds, the presentation mailbox
replaced 17 frames and expired another 17. Menu rendering occupied the shared
presentation path.

r114 enabled asynchronous video presentation under an unchanged menu layer.
It measured 58.59 unique FPS with the menu and 59.65 without it. That change
alone was insufficient.

r115 submits updated FPS, temperature, load and battery text together with the
next new video frame. Taps, page changes, notices and USB-loss status still update
immediately. If video stops, the next menu update flushes staged telemetry.

An inactive menu buffer cannot be overwritten while an earlier DECON
configuration still references it. Reuse waits for its retire fence. Queue sizes,
frame-age limits, power settings and FPS rounding are unchanged. No full-frame
CPU image copies were added.

## Device Measurements

A visible test source encoded a distinct frame number in each image.
It ran for 120 seconds at 60.00000 Present/s.

| Nested measurement window | Unique decoded FPS | Unique presentation requests/s |
| --- | ---: | ---: |
| Menu closed, about 24 s | 59.918 | 59.650 |
| Menu open, about 24 s | 59.866 | 59.528 |

The open-menu window had 24 menu rebuilds and zero additional submissions of
old video. Neither window had repeated or invalid test tags. Small gaps remain:
8 and 11 missing source IDs at presentation, respectively. SoC was 40-41 C,
with no decoder or DECON errors.

The display GPIO TE counter delivered approximately 59.61 interrupts/s over
108.65 seconds. A closed-menu interval measured TE and FrameDone at 59.638/s
alongside 59.650 unique submissions/s. These are delivered interrupt counts,
not optical measurements or proof of the panel's maximum refresh rate.
Exactly 60 new physical images every second remains unverified. Panel clocks
were not changed.

## Verification and Limits

- r115 BOOT was written with SHA-256 readback. The phone and PC agree on the
  unchanged Windows package. Camera capture, USB, audio and Windows binaries
  were not changed by these presentation updates.
- Race tests covered frame ownership, layer transitions, deferred telemetry,
  refusing writes to the active buffer and retaining memory after fence errors.
  Private and sanitized ARM64 builds passed.
- Display interrupt reads run only during the explicitly enabled bounded
  synthetic-frame test. They are disabled in normal operation.
- Test processes exited and the probe was disabled after measurement.
- The owner accepted this FPS result as sufficient. A separate full visual/input
  regression checklist was not recorded; acceptance of the result is not that test.
- These are 720p results. Long 1440p and simultaneous full-function acceptance
  remain open. This is not a production-release sign-off.
