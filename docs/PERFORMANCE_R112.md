# Monitor r112-r113: Separate Capture and Transport Rates

30 September 2026. Tested on one Samsung Galaxy S7 SM-G930F.
Historical report; see [r115](PERFORMANCE_R115.md) for the later menu changes.

## Changes

Windows Monitor 0.3.33.0 advertises a 120 Hz virtual display. This is the desktop
capture rate, not an S7 panel overclock. USB, H.264 and phone settings remain
at 60 FPS. The device owner approved this experiment. Other displays were not
reconfigured.

Two completed NV12 images form a bounded pre-encoder queue. Output pacing has
an average period of exactly 1/60 second. Overflow replaces only independent
raw images, never H.264 reference frames. The frame-age limit is unchanged.
Sniper retains its previous latest-frame queue and processing cadence.

Two unsynchronized cycles near 60 Hz had produced variable results: the same
test animation could reach nearly 60 FPS or only about 47 at the encoder input.
Extra capture opportunities test that case; they do not establish one cause for
every Windows frame-loss scenario.

## Installed Package

Package 2026093008, installer media and matching BOOT were written to the phone.
SHA-256 values were verified after writing. Windows installed the package from
the phone and confirmed the match. Camera, manager, Sniper helper and installer
binaries were preserved byte for byte.

Windows moved S7's desktop position during mode migration. A targeted call
restored only S7's previous position. Other displays' position, resolution,
refresh rate and orientation were checked and unchanged.

## Controlled Animation

The source uses a precise 60 FPS timer and a number in every frame. It requires
neither a video player nor manual focus changes.

| Test | Counter interval, s | Received | Decoded | Presentation requests |
| --- | ---: | ---: | ---: | ---: |
| First 120-second source | 111.9956 | 6708 | 6708 | 6674 |
| Second 120-second source | 112.1490 | 6705 | 6705 | 6675 |

Source rates were 60.00010 and 60.00004 Present/s. Reception was about
59.8-59.9 FPS and presentation requests about 59.5-59.6 FPS. Earlier 40-50 FPS
windows did not recur in these runs. There were no USB errors or decoder restarts.
SoC stayed at 40 C with the existing ECO settings. This is not a controlled
long-load temperature-reduction measurement.

A separate VSync source measured 60.17 received and 59.76 submitted FPS over a
short window. Non-atomic counter snapshots can produce slightly above-60 results.
The WGC observer saw changing image IDs but missed 74 itself; it was not accepted
as an exact end-to-end unique-frame counter.

## Interpretation Limits

- `PresentationFrameNumber` gaps are not necessarily lost pictures. A 120 Hz
  virtual display can have empty ticks between 60 FPS source updates.
- `cpu_blits` counts successful presentation requests, not OLED scanout.
- The phone still replaces or expires some pending frames. Exactly 60 unique
  physical images every second cannot be claimed.
- Long 1440p and combined-load acceptance is separate and incomplete.

## Code Checks

The full targeted Windows x64 build and tests passed. Coverage included FIFO
behavior, queue policy changes, frame expiry, rational 60 Hz pacing and
60/120 Hz EDID modes. Real RTX 3060 tests passed at 720p and 1440p: 239 GPU inputs
near 120 Hz, 120 encoded frames at 60 FPS and an IDR refresh of a retained image.
No CPU NV12 copying was added. The sanitized x64 build also passed.

## Unique Frames on S7

r113 BOOT was installed with SHA-256 readback. The Windows archive and installer
disk were unchanged and rechecked. A bounded counter reads a test tag from
completed decoded NV12 and after successful presentation submission. It is off
by default, explicitly enabled for at most 30 seconds, not persisted and saves
no images.

| Source | Unique decoded frames | Decoder FPS | Unique submissions | Submission FPS |
| --- | ---: | ---: | ---: | ---: |
| Precise 60 FPS timer | 1199 | 59.941 | 1194 | 59.705 |
| Precise timer with VSync | 1200 | 60.006 | 1193 | 59.650 |

Both approximately 20-second windows had zero repeated and zero invalid tags.
They prove changing images after the real H.264 decoder, not inflated counters
from repeating one image. Window boundaries and arrival timing account for small
variations near 60. The remaining 7 and 8 missing output IDs were not hidden.

The counter disabled itself after both windows. Final state: Monitor ready,
matching package, zero decoder retries, no errors, SoC 41 C. No test processes or
task-owned ETW sessions remained. Other display settings were unchanged.
Three targeted Go race runs passed; the known full-suite failures remained.
