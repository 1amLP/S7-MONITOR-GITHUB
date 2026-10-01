# PeriMode

**One old phone. Your everyday peripherals, together.**

PeriMode turns an old smartphone into a USB monitor, webcam, speaker,
microphone and touch surface. Sniper mode brings a selected part of your main
display onto the phone, with touch input, zoom, pan, rotation and mirroring.
The goal is simple: keep useful phones working instead of throwing them away.

This source tree targets **Samsung Galaxy S7 SM-G930F (herolte, Exynos 8890)**.
Monitor, both cameras, audio and Sniper Touch have been used on one real device.
Other phones, including S7 edge and Snapdragon variants, are **not supported
by this firmware**. Supporting them requires device-specific kernel, USB,
display, camera and power work. Reuse the architecture, not the S7 BOOT image.

## Modes

| Mode | Purpose |
| --- | --- |
| Monitor | A 720p or 1440p USB display for Windows |
| Camera | Front and rear camera sources for Windows applications |
| Preview | A local camera preview on the phone |
| Audio | Independently enabled speaker and microphone |
| Touch / Pad | Direct touch input or a touchpad for Windows |
| Sniper | A movable, zoomable region of the main display with hardware HID Touch |

See [Modes and controls](docs/MODES.md). Firmware menus and project documentation
are in English. Windows application language follows Windows settings.

## Release Status

**Experimental, device-specific source release. Not production-ready firmware.**
The current device build is r116 with Windows Monitor 0.3.33.0. It adds
[menu fixes](docs/MENU_R116.md) to the owner-accepted r115 presentation path.
In the controlled r115 720p run, it submitted 59.65 unique FPS without the menu and
59.53 with the menu. Small losses remain; this is not a guarantee of exactly
60 physical screen updates. Windows Camera may omit 120/240 FPS options.
Long 1440p and combined-load acceptance are still incomplete.

The release audit found six failing native test packages. The sanitized source
builds, but its complete signing, installation and upgrade cycle has not been
validated on another device. See the [release audit](docs/AUDIT_R115.md),
[current status](docs/STATUS.md) and [measured results](docs/PERFORMANCE_R115.md).

No BOOT images, vendor libraries, private device serials, certificates, signed
drivers, test binaries or diagnostic logs belong in this repository.
Building requires legally obtained files from **your own compatible S7**.
Read [Build and installation](docs/BUILD_INSTALL.md) before attempting a build.

Check the source tree before publication:

```sh
python3 tools/audit_source_tree.py --history
```

This checks file hygiene, English text, local links and common private-data
patterns. It does not replace tests, firmware acceptance or a security review.

## Compatibility and Licensing

- Tested hardware and porting limits: [Compatibility](docs/COMPATIBILITY.md).
- Project source is GPL-3.0. Third-party components retain their own terms:
  [Provenance](docs/PROVENANCE.md).
- The Go module uses `perimode/native`; it does not depend on a GitHub username.

**GitHub description:** Give an old phone a new role: USB monitor, webcam,
audio and touch in one device. Experimental support for Samsung Galaxy S7
SM-G930F, built as a foundation for keeping older phones useful.
