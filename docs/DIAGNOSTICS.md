# Diagnosing a Connection Loss

This document describes engineering builds made with `--ui-diagnostics`.
Diagnostics do not record screen, camera or audio media. Builds without this
flag do not retain the engineering USB-stall snapshots described below.

## On the Phone

The `usb-stall-report` worker watches Monitor. After a two-second link loss,
it can save `last-usb-stall.json`; complete USB disappearance is included.
Normal PC sleep and intentionally disabling USB from the menu do not replace
the fault snapshot.

The snapshot contains time, camera/USB state, frame and queue counters,
temperature, Windows package state, the last driver error, kernel messages
and worker stacks. It is bounded to 256 KiB, at most three writes per boot,
using one atomically replaced file. It is not continuous per-frame recording.
Writing requires active CACHE persistence.

If the diagnostic channel still responds, collect it before restarting.
If USB is unresponsive, boot TWRP and mount CACHE, then run:

```sh
adb -s <SERIAL> pull /cache/s7-native/last-usb-stall.json <LOCAL_FAULT_DIRECTORY>
adb -s <SERIAL> pull /cache/s7-native/last-exit-before-stop.json <LOCAL_FAULT_DIRECTORY>
```

Keep these files until the fault is understood. They may contain the device
serial or other local information. Do not commit or publish them without review.

## On Windows

`S7Monitor` and `S7Camera` write to the **Application** event log. Events cover
lifecycle, errors and aggregate counters, not media. Monitor records the original
Windows error, USB frame size, acknowledged bytes, elapsed time and connection
generation. Camera records starts, stops, capture errors and five-second frame
summaries. Windows logs are bounded and older events may be overwritten.

This collector reads events and PnP state without restarting services or changing
drivers:

```powershell
./host-windows/diagnostics/Collect-Fault.ps1 `
  -OutputDirectory <NEW_LOCAL_DIRECTORY_OUTSIDE_GIT> -Minutes 30
```

`QueryErrors` lists inaccessible logs. An empty event list does not prove that
no failure occurred. Check the PC and phone clock offsets when comparing events.

## Bounded Frame Tests

The optional synthetic-frame counter is off by default, is not persisted and
runs for at most 30 seconds after an explicit command. It recognizes a test
pattern in decoded NV12 without saving images. During that test, r115 can also
read DECON interrupt counts. Both are inactive in normal use.

Submission counts and delivered TE interrupts are not optical measurements of
the panel. A test source, a WGC callback and a physical screen update are different
observations. See [measurement limits](PERFORMANCE_R115.md).

## Remaining Limits

A short camera-plus-Monitor test passed. The owner also reported a successful
WhatsApp call after r103. Multi-hour combined-load acceptance on the latest
build remains open. The DWC3 fix addresses the identified NULL `resume` callback
panic during USB wakeup; it does not prove that all disconnects shared that cause.
