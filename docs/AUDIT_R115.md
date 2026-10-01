# Release Readiness Audit

**Date:** 1 October 2026. **Verdict:** usable engineering baseline for the tested
owner's S7; clean source for private publication; **not a production firmware
or installer release**.

Baseline: r115, Windows Monitor 0.3.33.0, package 2026093008, source `d176bdb`.
The owner accepted the current FPS result. This audit did not replace firmware,
drivers, signing certificates or camera settings on the working device.

**Follow-up:** after the audit, the owner authorized [r116 menu fixes](MENU_R116.md).
The control-queue and invalid-camera-bank findings below describe the pre-fix r115
baseline; those two paths now have fixes and targeted verification. r116 was
installed without changing the Windows package. The remaining release gates and
the not-production-ready verdict still apply.

## Ratings

These are engineering judgments, not certified or statistically calibrated scores.

| Area | Rating | Basis |
| --- | ---: | --- |
| Source-tree hygiene | 9/10 | Product source and documentation, no detected private device data or built artifacts in the reviewed tree; repeatable hygiene checks added |
| Daily use on the tested S7/PC | 7/10 | Main features were used successfully; owner-accepted performance, but recovery edge cases and long-load gaps remain |
| Build reproducibility | 6/10 | Fresh sanitized Windows x64 and Linux ARM64 builds pass; vendor inputs, exact kernel pins and signing remain operator-specific |
| Release regression coverage | 3/10 | Six native packages fail the engineering suite; the sanitized tree intentionally excludes those fixtures |
| Maintenance and security lifecycle | 3/10 | Reproducibility toolchain is old; ongoing vendor-kernel patch coverage is not established |
| Ready-to-install public release | 3/10 | No accepted sanitized signing/install/upgrade cycle, one tested handset, engineering diagnostics in the installed baseline |
| Portability to other phones | 2/10 | Hardware-specific kernel, display, ISP, audio, USB and power contracts; no accepted second device |

Removing old files improves hygiene. It does not fix failed tests or establish
hardware, security, signing or redistribution acceptance.

## Release Blockers

### P1: The reproducibility toolchain is not maintained

The retained native compiler is Go 1.23.2. It reproduces the current engineering
build, but it is outside Go's supported release window. Go maintains a major
version only until two newer major versions exist; the official release history
now includes 1.26 and 1.27. See the [Go release policy](https://go.dev/doc/devel/release).
Keeping this compiler for reproducibility is not recommending it for a new
production release. A supported compiler needs compatibility and hardware
validation before replacing the installed binary.

The phone also uses a vendor-modified Linux 3.18 kernel. It is not on the current
[upstream longterm list](https://www.kernel.org/category/releases.html).
Vendor backports and the two reviewed local fixes do not establish ongoing
security maintenance. This audit did not perform a complete CVE or exploit review.

### P1: The full native regression suite is not green

A fresh `go test -json -count=1 -timeout=60s ./...` in the engineering source tree
reported 29 passing packages, 6 failing packages and 8 without tests. Failures:
`appliance`, `audio`, `fimg2d`, `selinux`, `uvcout`, and `pkg/hid`.

`TestCamera3AInvalidSavedSelectionCanBeReset` panicked on an obsolete row index.
That stops the remainder of its package, so the audit cannot claim every appliance
test executed. Several failures are stale expectations: old audio queue bounds,
G2D scaling, HID dispatch ownership and the replaced UVC path. They still leave
the release without a trustworthy all-green regression gate. Do not delete or
weaken a check merely to obtain a passing result.

The public `Build-Package.ps1` uses `-SkipTests` because engineering fixtures are
excluded from the source distribution at the owner's request. Its successful
build is not a successful test run. See [build procedure](BUILD_INSTALL.md).

### P1: The distributable installation path has not passed acceptance

The sanitized source takes a target serial and publisher identity at build time.
Fresh Windows and ARM64 candidates compile, but the complete sanitized
signing, installation, update, rollback and second-PC workflow has not been
accepted. The working owner's package is not proof that another builder's package
is compatible or trusted by Windows.

BOOT and Recovery checks pin specific images. Vendor inputs and the exact
kernel build must be provided legally and matched correctly. A public ready-made
release must resolve input provenance, signing and supported-device policy.
This audit does not grant permission to redistribute Samsung binaries.

### P2, fixed in r116: A busy control queue could reject the final action

In the r115 baseline, `UI.async` performed a nonblocking send and reported
`control operation already pending` on overflow. Camera parameter edits used
this path. Under contention, the user's last requested value could be lost.
The error was subsequently observed on the working r115 phone before replacement.
The [r116 queue](../native/internal/appliance/control_queue_linux.go) coalesces
named settings separately from one-shot commands. See [verification](MENU_R116.md).

### P2, fixed in r116: Invalid saved camera controls lacked a menu reset

In r115, `camera3ALines` emitted control rows only when the sensor-specific bank
was valid. Otherwise it offered Back without a reset. A generic-valid saved
selection could violate a sensor limit and prevent camera startup. This recovery
defect was separate from the stale test's indexing panic. The current
[camera controls](../native/internal/appliance/camera_3a_linux.go) include a
sensor/mode-specific reset, covered by [r116 checks](MENU_R116.md).

### P2: Timing and hardware acceptance remain incomplete

- r115 measured 59.650 unique submissions/s with the menu closed and 59.528
  open in a controlled 720p run. Small frame losses remain.
- The menu worker still holds shared presentation ownership while rendering.
  Combining telemetry with video removed extra old-frame submissions but did
  not eliminate all lock occupancy or prove optimal scheduling.
- TE counters measured about 59.61 delivered interrupts/s. This is not an optical
  panel measurement or proof of a hardware ceiling.
- Long 1440p, simultaneous Monitor/camera/audio/Sniper load, overnight PC startup,
  repeated USB sleep/wakeup and long battery-policy acceptance remain open.
- The earlier 240 FPS camera test was not lossless. Windows Camera can omit
  high-speed options; other clients exposing them is not universal acceptance.

### P2: Installed firmware remains an engineering build

The installed baseline includes `--ui-diagnostics`. Its USB command channel can
inspect state and request bounded diagnostic/UI/recovery operations. It is not
an arbitrary shell, but it is more than a production data-only interface. The
normal builder defaults this flag off. Decide the release policy explicitly and
validate that build; do not relabel the installed engineering image as production.

## Checks Performed Now

- Working phone: Monitor active, package 2026093008 verified, camera waiting
  rather than capturing, no current runtime error, synthetic probe disabled.
- Fresh sanitized Windows x64 package build: passed, with INF/catalog checks.
  Candidate is unsigned, uninstalled and not hardware-accepted.
- Sanitized native `go vet ./...`: passed.
- Sanitized ARM64 build after artifact cleanup and WSL relocation: passed.
- Full engineering native suite: failed as described above. The recent targeted
  presentation/ownership race checks from r115 remain separately recorded.
- Reviewed tracked files for private serials, user paths, key/token signatures
  and accidentally tracked binaries/logs. No matches in the pre-translation tree.
  `tools/audit_source_tree.py` provides the repeatable final tree/history checks.
- Firmware, Windows component source and current status strings were already
  English. No language-only flash was needed. Documentation was translated;
  historical commits were not rewritten.

Pattern scans cannot prove the absence of every secret or personal detail.
Source builds cannot prove behavior on another device. No camera/audio recording,
new UAC signing cycle or phone reflash was performed during this audit.

## Minimum Release Gates

1. Repair current-contract regression tests and any remaining confirmed defects;
   obtain an honest green suite without hiding unsupported cases. The two menu
   defects above are fixed, but the final r116 run still has six failing packages.
   Validate a maintained compiler and document the vendor-kernel maintenance policy.
2. Freeze the supported model, BOOT/Recovery/kernel inputs, USB protocol and
   signing identity. Produce one versioned, internally consistent package.
3. Validate clean installation, update, failure recovery and rollback from the
   sanitized source on a separate PC and a matching second handset.
4. Run long combined-load, 1440p, reconnect, sleep/wakeup, PC-startup and charging
   tests with bounded diagnostics and clear pass criteria.
5. Review vendor/kernel redistribution obligations and sign the distribution
   through a legitimate, supported Windows process.

Until these gates pass, describe PeriMode as an **experimental SM-G930F source
project**, not a universal old-phone appliance or a production installer.
