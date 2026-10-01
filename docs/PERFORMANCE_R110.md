# Monitor FPS: r109-r111

30 September 2026. One Samsung Galaxy S7 SM-G930F.
Historical results, not proof of stable 60 FPS for every application.

## Installed Changes

- Monitor 0.3.32.0 and package 2026093007; phone/PC package match confirmed.
- BOOT and installer media contain the same package.
- Camera, Sniper, audio and kernel were not redesigned in these changes.
- Build uses IddCx 1.9 and retains a 1.4 minimum. The GPU-priority request is
  guarded by API availability and restores the previous state.
- Active acquisition retries after `E_PENDING` use a precise 2 ms timer.
  After 100 ms idle, the interval returns to 50 ms. Global Windows timer
  resolution is unchanged.

The older ordinary 16 ms delay sometimes lasted about 32 ms. That long pause
did not recur in r110. This is a partial timing fix, not an FPS guarantee.

## Controlled Source

The S7 test animation carries a binary frame ID and its complement. A precise
timer sets the source rate; no ordinary video or manual user actions are needed.

The 120-second source produced 60.00002 Present/s. Phone counters over a nested
111.8608-second interval were:

| Stage | Frames | FPS |
| --- | ---: | ---: |
| Received | 6710 | 59.985 |
| Decoded | 6710 | 59.985 |
| Presentation requests | 6675 | 59.672 |

Driver logs showed about 300 new IDs per five seconds without gaps or repeats
in those windows. CPU readbacks and GPU allocator waits were zero. Decoder
retries and transport drops did not increase. SoC stayed at 40 C. These are not
physical panel measurements.

The decoder discarded 7 completed frames, the mailbox replaced 20 pending
frames and expired 7. Boundary frames and non-atomic snapshots prevent exact
one-frame reconciliation. The output shortfall is not rounded away.

## Limits

- Ordinary video in r110 still produced 50.4-59.6 FPS output windows.
- An active S7 window also fell to 47 FPS. Window focus was not established as
  the cause. Other displays' refresh rates were untouched.
- A separate trace matched 1675 DirectComposition S7 frame IDs to IddCx;
  all those IDs reached the driver. This locates loss in that run but does not
  establish its internal OS cause.
- A Present call, WGC callback or new frame ID is not proof of a distinct
  physical image. Short pixel-counter tests were separate, and the WGC observer
  itself missed frames.
- Source Present policy affected results. One successful short synthetic test
  is not whole-product acceptance.

## Builds

The private Windows x64 build and targeted tests passed, including an IddCx 1.4
build. Sanitized Monitor x64 compiled with a synthetic serial, `/W4 /WX`, INF
validation and catalog generation. It was not signed or installed. Known
full-suite failures from [r103](AUDIT_R103.md) remained.

The installer does not launch the diagnostic programs or traces. Tests are
bounded and save no video. Raw logs, captures, serials, local paths, signed
packages and keys are not part of this source repository.

API references: [Compositor clock](https://learn.microsoft.com/en-us/windows/win32/directcomp/compositor-clock/compositor-clock)
and [IddCx metadata](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/iddcx/ns-iddcx-iddcx_metadata).

## r111: Ordered Decoded Frames

r111 passes completed decoder bursts into the existing bounded presentation
mailbox. `DrainLatest` previously discarded frames before that mailbox.
Queue size, age limit and DMA ownership are unchanged. The old direct path
without a mailbox retains its previous behavior. Windows remains at r110.
BOOT readback, package and installer-media hashes were verified.

A new two-DMA-frame burst regression failed before the change. Three targeted
decoder/mailbox/DECON race runs then passed, as did both ARM64 builds. A broader
selection included the failing `TestTorchWorkerCancellationClosesDriver`, which
does not call the changed decoder. The full suite was not declared green.

A repeat with the same source binary produced 59.99992 Present/s. Over 112.0741 s,
S7 received and decoded 6479 frames and submitted 6452. Early decoder discard
was zero; the mailbox replaced 8 and expired 18. No USB errors or decoder restarts
occurred; SoC was 40-41 C.

Windows acquisition nevertheless started near 47 FPS and later approached 60
while the source stayed at 60. The main drop therefore reproduced without a
browser or video. Different input cadences make the two run averages unsuitable
as a direct firmware-speed comparison.
