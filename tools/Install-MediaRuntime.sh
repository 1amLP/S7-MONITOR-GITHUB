#!/system/bin/sh
# One-time RECOVERY-only installation. This script is never run by the appliance.
# No flash/format/remount/ADB/driver/certificate changes. Only /cache/s7-media.
set -eu
[ "${1:-}" = '--allow-cache-write' ] || { echo 'Usage: sh Install-MediaRuntime.sh --allow-cache-write RUNTIME.tar.gz SHA256 SERIAL'; exit 2; }
[ "$#" = 4 ] || exit 2
archive=$2; sha=$3; serial=$4
case "$sha" in *[!0-9a-f]*|'') echo 'Invalid SHA256'; exit 2;; esac
[ "${#sha}" = 64 ] || exit 2
[ "$(id -u)" = 0 ] || { echo 'Recovery root required'; exit 2; }
# Refuse ordinary Android and our running appliance. Installation is an explicit
# maintenance step before flashing the matching BOOT, not a normal-use PC app.
[ -x /sbin/recovery ] || [ -x /system/bin/recovery ] || [ -d /twres ] || { echo 'Run only in recovery'; exit 2; }
[ -f "$archive" ] && [ ! -L "$archive" ] || exit 2
[ ! -L /cache ] && [ -d /cache ] || exit 2
line=$(awk '$2=="/cache" && $3=="ext4" {print $1 " " $4}' /proc/mounts)
[ -n "$line" ] || { echo 'Mount CACHE in recovery first. No mount attempted.'; exit 2; }
case "$line" in *' rw,'*|*' rw') :;; *) echo 'CACHE is not writable'; exit 2;; esac
dev=${line%% *}; dev=$(readlink -f "$dev")
expected=$(readlink -f /dev/block/platform/155a0000.ufs/by-name/CACHE)
[ -n "$expected" ] && [ "$dev" = "$expected" ] || { echo 'Wrong CACHE block device'; exit 2; }
actual=$(tr ' ' '\n' < /proc/cmdline | sed -n 's/^androidboot.serialno=//p' | head -n 1)
[ -n "$actual" ] || actual=$(cat /sys/devices/soc0/serial_number 2>/dev/null || true)
[ "$serial" = "$actual" ] || { echo 'Phone serial mismatch'; exit 2; }
computed=$(sha256sum "$archive"); [ "${computed%% *}" = "$sha" ] || { echo 'Archive hash mismatch'; exit 2; }
base=/cache/s7-media
[ ! -L "$base" ] || exit 2
mkdir -p "$base"; chmod 700 "$base"
dest="$base/runtime-$sha.tar.gz"; consent="$base/consent-$sha"
[ ! -L "$dest" ] && [ ! -L "$consent" ] || exit 2
if [ -e "$dest" ]; then
 [ -f "$dest" ] || exit 2
 computed=$(sha256sum "$dest"); [ "${computed%% *}" = "$sha" ] || { echo 'Existing different file; preserved'; exit 2; }
else
 tmp="$base/.runtime-$$.tmp"; [ ! -e "$tmp" ] && [ ! -L "$tmp" ] || exit 2
 trap 'rm -f "$tmp"' EXIT
 (umask 077; set -C; cat "$archive" > "$tmp")
 computed=$(sha256sum "$tmp"); [ "${computed%% *}" = "$sha" ] || exit 2
 sync
 mv "$tmp" "$dest"; sync
 trap - EXIT
fi
# Publish the narrowly scoped consent LAST. Old version files are never deleted.
if [ -e "$consent" ]; then
 [ -f "$consent" ] || exit 2
 expected_consent=$(printf 'S7-NATIVE-MEDIA-1\n%s\n%s\n' "$serial" "$sha")
 [ "$(cat "$consent")" = "$expected_consent" ] || exit 2
else
 tmp="$base/.consent-$$.tmp"; [ ! -e "$tmp" ] && [ ! -L "$tmp" ] || exit 2
 trap 'rm -f "$tmp"' EXIT
 (umask 077; set -C; printf 'S7-NATIVE-MEDIA-1\n%s\n%s\n' "$serial" "$sha" > "$tmp")
 sync; mv "$tmp" "$consent"; sync
 trap - EXIT
fi
echo 'Runtime stored. BOOT was NOT flashed. Android services were NOT started.'
echo 'Use only the BOOT whose /etc/s7-codec/runtime.json pins this exact SHA256.'
