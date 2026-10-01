# Build and Installation

This procedure applies only to the [reviewed hardware](COMPATIBILITY.md).
The repository does not provide ready-to-flash firmware or signed drivers.
Do not flash another phone's BOOT or disable Windows signature enforcement.

**This is a source-build procedure, not a validated public installer release.**
Read [the audit](AUDIT_R115.md) before attempting deployment.

## 1. Inputs and Recovery

Required: Linux/WSL with Go and Python 3; Windows 11 x64 with MSVC 2022,
Windows SDK/WDK and PowerShell 7.2 or later; TWRP; and legally obtained files
from your own compatible SM-G930F.

The module's language minimum is Go 1.23. The retained engineering build used
Go 1.23.2 for reproducibility; that version is no longer maintained. A production
build must validate a supported compiler instead of treating this historical
pin as a security recommendation. See [the audit](AUDIT_R115.md).

Keep original BOOT, RECOVERY and device recovery backups outside the phone.
Before writing, verify the BOOT, kernel, DTB and Recovery pins in the code and
[compatibility document](COMPATIBILITY.md). `tools/native_boot.py` deliberately
rejects incompatible inputs. Never change a hash merely to bypass the check.

Populate `hardware/firmware`, `hardware/sensorhub`, `hardware/mediacodec`,
`hardware/gpu` and `hardware/fonts` locally from permitted sources. Git ignores
these directories. Helper build procedures are in `tools/build_*.py`.

`<SERIAL>` below means the target phone's serial from `/proc/cmdline`, exactly
18 hexadecimal characters. Do not put it in a commit, issue or public screenshot.

## 2. Native Phone Code

From `native`:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false \
  -ldflags '-X perimode/native/internal/appliance.PinnedSerial=<SERIAL>' \
  -o <OUTPUT_OUTSIDE_REPO>/s7-native ./cmd/s7-native
```

An empty `PinnedSerial` intentionally prevents USB binding. A build without
`-X` is only a compile check, not a deployable image.

## 3. Windows Components

`host-windows/Build-Package.ps1` produces an **unsigned candidate**. Supply
your MSVC/SDK/WDK paths, a new output directory outside Git and
`-DeviceSerial <SERIAL>`. It builds Monitor, Camera and the diagnostic FPS tool.
It does not create trust, a private key or a signature-policy bypass.

The source distribution excludes engineering test fixtures. Its package build
therefore uses `-SkipTests`; build success must not be presented as a full test
pass. Current engineering-suite failures are recorded in [the audit](AUDIT_R115.md).

The candidate, `S7Setup.exe` and `S7PackageCheck.exe` require signatures from an
already trusted publisher. `host-windows/Seal-Candidate.ps1` fixes the exact
bytes. `host-windows/Build-PhonePackage.ps1` takes `-DeviceSerial <SERIAL>`,
`-PublisherThumbprint <THUMBPRINT>`, `-Release <NUMBER>` and signed inputs.
The serial is included only in the local signed package.

`host-windows/Build-InstallerMedia.ps1` and `tools/build_install_media.py`
produce the read-only installer disk and `PHONE_PACKAGE.json` outside Git.
Local trust on the developer's PC does not establish trust on other computers.

## 4. BOOT Image

After preparing the local hardware inputs and package pin:

```sh
python3 tools/native_boot.py \
  --input-boot <OWN_MATCHING_BOOT.img> \
  --init <OUTPUT_OUTSIDE_REPO>/s7-native \
  --firmware-dir hardware/firmware \
  --output-dir <NEW_OUTPUT_OUTSIDE_REPO> \
  --direct-mfc --windows-package-pin <PHONE_PACKAGE.json>
```

Check `BOOT_BUILD.json`: source/output SHA-256, the 41943040-byte BOOT size,
package pin and unchanged kernel/DTB when no replacement was supplied.
The builder does not flash the phone. The image includes locally supplied
firmware material and must not be committed to GitHub.

For the reviewed DWC3/MFC kernel, add:

```sh
--kernel-image <PINNED_KERNEL_IMAGE> \
--kernel-pin hardware/source-config/kernel-mfc-cache.json
```

The builder validates source/configuration pins, SHA-256, ARM64 header and
kernel release. Expect `dtb_unchanged=true` and `kernel_and_dtb_unchanged=false`
when replacing the kernel. The repository contains patches and pins, not a
complete distributable kernel tree or binary. An independent build can have a
different hash and is not covered by the existing pin.

`mfc-source-cache.patch` applies over the DWC3-fixed kernel with
`git apply --unidiff-zero`. It changes compressed H.264 input cache synchronization
to use `bytesused`. Uncompressed CAPTURE buffers, DRM and the camera encoder
are unchanged. The older USB-wakeup pin remains for reproducibility. The original
kernel has a reproduced NULL callback panic and is not the fixed release.

Do not enable `--ui-diagnostics` for a normal release without explicitly deciding
to ship the engineering command channel. The installed development baseline has
it enabled; changing that flag requires its own deployment check.

## 5. Install on the Reviewed S7

Boot TWRP. Verify serial and current BOOT/RECOVERY hashes. Stage the candidate,
a verified previous BOOT and `tools/Install-NativePersistent.sh` in a new TWRP
RAM directory. The script accepts:

```sh
sh Install-NativePersistent.sh --install-native <NEW_BOOT_SHA256> <OLD_BOOT_SHA256> <SERIAL>
```

It rejects unexpected partitions, preserves the previous image, verifies the
write by reading it back and only then reboots. Do not run without a separate,
verified recovery copy. The script pins a specific Recovery image, not arbitrary
TWRP versions.

A new signed PC package needs a separate SYSTEM update:

```sh
sh Install-PhonePackage.sh --stage-signed-package \
  <NEW_ZIP_SHA256> <OLD_ZIP_SHA256|none> \
  <NEW_MEDIA_SHA256> <OLD_MEDIA_SHA256|none> <SERIAL>
```

The sanitized version of this procedure has not passed a full signing/install/
upgrade cycle. Do not apply it to an active device without separate validation
and a rollback plan. BOOT, package archive and installer media must agree.

After boot, expose the installer disk explicitly through
`Device > Install Driver`, then run `S7Setup.exe` from it on Windows. Check Monitor,
both cameras, camera switching/Zoom, Audio/Mic and Sniper Touch. A USB-ready status
alone is not full acceptance of the package or its features.

## 6. Publish Source Only

Use GitHub Desktop: **File > Add Local Repository**, then **Publish repository**.
Use **PeriMode** as the name and **Keep this code private** until the release
checks are complete. Do not publish local hardware material, BOOT, logs,
captures, keys or signed packages. Historical audit reports remain useful;
they are not claims that their older builds are current.
