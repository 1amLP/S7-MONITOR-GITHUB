#!/sbin/sh
set -eu

# Recovery provisioning only. Normal native boot serves this file read-only.
# This script does not install Windows software or flash BOOT/Recovery.
[ "$#" = 6 ] && [ "$1" = '--stage-signed-package' ] || exit 2
new_sha=$2
old_sha=$3
media_sha=$4
old_media_sha=$5
serial=$6
[ "${#serial}" = 18 ] || exit 3
case "$serial" in *[!0-9a-fA-F]*) exit 3 ;; esac
case "$media_sha" in *[!0-9a-f]*|'') exit 3 ;; esac
[ "${#media_sha}" = 64 ] || exit 3
if [ "$old_media_sha" != none ]; then
    case "$old_media_sha" in *[!0-9a-f]*|'') exit 3 ;; esac
    [ "${#old_media_sha}" = 64 ] || exit 3
fi
case "$new_sha" in *[!0-9a-f]*|'') exit 3 ;; esac
[ "${#new_sha}" = 64 ] || exit 3
if [ "$old_sha" != none ]; then
    case "$old_sha" in *[!0-9a-f]*|'') exit 3 ;; esac
    [ "${#old_sha}" = 64 ] || exit 3
fi
[ -x /sbin/recovery ] && [ -d /twres ] || exit 4
tr ' ' '\n' </proc/cmdline | grep -Fxq "androidboot.serialno=$serial" || exit 4
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
candidate="$root/package.zip"
[ -f "$candidate" ] && [ ! -L "$candidate" ] || exit 5
bytes=$(stat -c %s "$candidate")
[ "$bytes" -gt 0 ] && [ "$bytes" -le 134217728 ] || exit 5
digest() { sha256sum "$1" | awk '{print $1}'; }
[ "$(digest "$candidate")" = "$new_sha" ] || exit 6
media="$root/install.img"
[ -f "$media" ] && [ ! -L "$media" ] || exit 6
[ "$(stat -c %s "$media")" -le 268435456 ] || exit 6
[ "$(digest "$media")" = "$media_sha" ] || exit 6

system=/dev/block/platform/155a0000.ufs/by-name/SYSTEM
[ -b "$system" ] || exit 7
real=$(readlink -f "$system")
if grep -q "^$real " /proc/mounts || grep -q "^$system " /proc/mounts; then
    echo 'SYSTEM already mounted; provisioning refused'
    exit 8
fi
mountpoint="$root/system-mount"
[ ! -e "$mountpoint" ] || exit 9
mkdir "$mountpoint"
mounted=false
cleanup() { if [ "$mounted" = true ]; then sync; umount "$mountpoint"; fi; }
trap cleanup EXIT
mount -t ext4 -o ro,noload,nodev,nosuid "$system" "$mountpoint"
mounted=true
blob="$mountpoint/system/vendor/lib64/egl/libGLES_mali.so"
[ -f "$blob" ] && [ ! -L "$blob" ] || exit 10
[ "$(digest "$blob")" = c9803ca74aa6b5d0c25fbad045483cf4342c717cd8f62b771a7b46c1bd4c0500 ] || exit 10
dir="$mountpoint/s7-windows"
target="$dir/package.zip"
[ ! -L "$dir" ] && [ ! -L "$target" ] && [ ! -L "$dir/install.img" ] || exit 11
if [ "$old_media_sha" = none ]; then
    [ ! -e "$dir/install.img" ] || exit 12
else
    [ -f "$dir/install.img" ] && [ "$(digest "$dir/install.img")" = "$old_media_sha" ] || exit 12
    [ ! -e "$root/install-previous.img" ] || exit 12
    cp "$dir/install.img" "$root/install-previous.img"
    [ "$(digest "$root/install-previous.img")" = "$old_media_sha" ] || exit 12
fi
if [ "$old_sha" = none ]; then
    [ ! -e "$target" ] || exit 12
else
    [ -f "$target" ] && [ "$(digest "$target")" = "$old_sha" ] || exit 12
    [ ! -e "$root/package-previous.zip" ] || exit 12
    cp "$target" "$root/package-previous.zip"
    [ "$(digest "$root/package-previous.zip")" = "$old_sha" ] || exit 12
fi
umount "$mountpoint"
mounted=false
mount -t ext4 -o rw,nodev,nosuid "$system" "$mountpoint"
mounted=true
mkdir -p "$dir"
[ ! -e "$dir/package-$new_sha.tmp" ] || exit 13
[ ! -e "$dir/install-$media_sha.tmp" ] || exit 13
cp "$candidate" "$dir/package-$new_sha.tmp"
cp "$media" "$dir/install-$media_sha.tmp"
chmod 444 "$dir/package-$new_sha.tmp"
chmod 444 "$dir/install-$media_sha.tmp"
sync
[ "$(digest "$dir/package-$new_sha.tmp")" = "$new_sha" ] || exit 14
[ "$(digest "$dir/install-$media_sha.tmp")" = "$media_sha" ] || exit 14
mv "$dir/install-$media_sha.tmp" "$dir/install.img"
mv "$dir/package-$new_sha.tmp" "$target"
sync
[ "$(digest "$target")" = "$new_sha" ] || exit 15
[ "$(digest "$dir/install.img")" = "$media_sha" ] || exit 15
umount "$mountpoint"
mounted=false
echo "SYSTEM_PACKAGE_READBACK_OK=$new_sha"
echo "SYSTEM_INSTALLER_READBACK_OK=$media_sha"
echo 'BOOT not changed; only a BOOT with this package pin should be installed next'
