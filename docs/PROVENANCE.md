# Source and redistribution

PeriMode is a GPL-3.0 source derivative of
`https://github.com/twsnmp/go-zerokvm` and the separately supplied S7 Monitor
development sources. The GPL-3.0 license texts are retained in this tree.
Generated UI masks retain their source notice in
`native/internal/fb/UI_MASKS_NOTICE.txt`.

This repository has a new, clean history. It intentionally excludes Samsung
and LineageOS firmware, vendor libraries, device partitions, signing keys,
signed drivers, private serial numbers, diagnostic logs and user images.
Hashes of required firmware inputs are compatibility checks, not grants to
redistribute those inputs. The attached green character image was not added:
its rights and relation to the code were not established.

`hardware/source-config/dwc3-wakeup.patch` is a one-line derivative of the
GPL-2.0-only DWC3 gadget driver in the supplied S7 kernel tree. The patch
retains the original copyright and author notice. It is not a
complete kernel source distribution or a binary. Its JSON pin identifies
the locally audited build; copying that pin is not hardware acceptance.

Build from firmware legally obtained for the builder's own compatible device.
Do not publish a BOOT or Windows driver package as a GitHub Release until its
contents, licenses, signatures and hardware checks have been reviewed.
