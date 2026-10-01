# PeriMode Modes

Home opens and closes settings. Power opens the power-off, restart and Recovery
menu. Settings persist on the phone. Monitor remembers its Touch/Pad choice
when returning from another mode. Menus and firmware labels are in English.

## Monitor

Windows sees an additional USB display with 1280x720 and 2560x1440 modes.
Since Monitor 0.3.33, the virtual Windows display advertises 120 Hz for capture;
USB video and phone presentation still target 60 FPS. This does not overclock
the S7 panel to 120 Hz. The tested 720p path reaches about 60 unique decoded
frames/s and roughly 59.5-59.7 presentation requests/s. See
[r112-r113 measurements](PERFORMANCE_R112.md) and [r115 menu results](PERFORMANCE_R115.md).

Touch provides direct input; Pad acts as a touchpad. The physical volume keys
control Windows volume, including when the phone's Speaker is enabled.

## Camera and Preview

Front and rear cameras are available to Windows applications. Camera switching
and digital zoom worked on the tested S7 after the earlier fixes. Preview shows
the camera locally. Taps in fullscreen Preview control focus, not the mouse.
The camera activity LED turns off when capture stops.

High-speed 120/240 FPS profiles are not accepted as universal end-to-end modes.
They may be absent from Windows Camera even when another client exposes them.
Do not advertise all modes as lossless or stable without measuring the full path.

## Audio

Speaker and Microphone are enabled independently. A level of zero disables that
endpoint. Select the device in Audio, then adjust its level in the detail column.
The phone's volume keys continue to control Windows rather than changing this
device-level setting.

## Touch, Pad and Sniper

Sniper displays a selected region of the main Windows screen. One finger sends
hardware HID Touch to the corresponding point. Two-finger gestures zoom, pan
and rotate the region. Pan becomes slower at higher zoom for precise positioning.
Recents mirrors the image horizontally. Home still opens settings. The Sniper
settings page contains its Enabled switch; the obsolete bottom controls were
replaced by gestures. Sniper Touch was confirmed to work after focusing Task
Manager on the tested PC. This does not establish compatibility with every app.

## Device

Device contains brightness, haptic strength and access to the Windows installer
disk. A haptic level of zero disables vibration. Menu feedback belongs to an
accepted interaction or value change, not every contact with empty space.

The setup disk is exposed only through an explicit menu action. An installed
Windows component can separately deliver and verify a matching package; an
update status is not proof that every component finished successfully.

The 50-80% charging policy applies to normal native operation, not TWRP. Long
battery and thermal acceptance remains incomplete. Do not leave recovery running
and assume the native charging policy is active.
