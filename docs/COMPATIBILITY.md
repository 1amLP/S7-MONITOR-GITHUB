# Compatibility

## Tested Device

- Samsung Galaxy S7 **SM-G930F**, codename **herolte**, **Exynos 8890**.
- The tested unit used the `G930FXXS1DRA5` bootloader family and TWRP Recovery.
- Pinned original BOOT SHA-256:
  `6544f30ceb74fda8be35d55664dd3570bdd5a00f453594ed1d488dab7c54233e`.
- Original kernel: `3.18.140-ge41817ea9198`, SHA-256
  `7d73f82eac469c5dc1e141d5ec0f28f208a0c91449ff85acc868f5a1f2dfbc01`.
- DTB SHA-256: `0c180a7249d70e7a4623a7a977a5f552670aa978fe981b992557e77c5349318a`.
- Earlier USB-wakeup fix: `3.18.140-gfd26b7e36c45`, SHA-256
  `dad80632e86cd915c28cce6040d50edc1f05dfa7cb6d5c9795d8c22cdb7d91cc`.
  See `hardware/source-config/kernel-usb-wakeup.json` and `dwc3-wakeup.patch`.
- Current kernel: `3.18.140-g3dfe42cdf48c`, SHA-256
  `1a9e09deafe49e510ce5ca8aece2a6759d6bcc52c2c326ffc2261ac446519b17`.
  See `hardware/source-config/kernel-mfc-cache.json` and `mfc-source-cache.patch`.
  The DWC3 fix remains present and the DTB is unchanged.

These hashes identify the exact reviewed build inputs. They do not establish
support for every phone labelled S7. The owner's serial is not published; use
the target device's own serial locally when building.

## Not Validated

S7 edge, Snapdragon variants, other S7 revisions, other kernels, locked
bootloaders, Windows ARM64 and other phone models have not passed hardware
acceptance. Do not flash the SM-G930F BOOT onto them. The recovery installer also
pins one specific Recovery hash; having TWRP installed is not sufficient.

## Porting to Other Phones

A port needs its own BOOT/Recovery inputs, backups and recovery procedure.
Validate kernel interfaces and addresses, hardware codecs, display controller,
USB gadget support, touch, audio, camera/ISP, battery and thermal sensors.
The Windows protocol and UI organization can provide a starting point.
S7 hardware tables and firmware blobs are not a portable hardware abstraction.
