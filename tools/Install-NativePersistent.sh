#!/sbin/sh
set -eu

# TWRP-only BOOT update. The host verifies kernel/DTB and the built image first.
[ "${1:-}" = '--install-native' ] && [ "$#" = 4 ] || exit 2
candidate_sha=$2
previous_sha=$3
serial=$4
[ "${#serial}" = 18 ] || exit 3
case "$serial" in *[!0-9a-fA-F]*) exit 3 ;; esac
for digest in "$candidate_sha" "$previous_sha"; do
    [ "${#digest}" = 64 ] || exit 4
    case "$digest" in *[!0-9a-f]*) exit 4 ;; esac
done
[ -x /sbin/recovery ] && [ -d /twres ] || exit 5
tr ' ' '\n' </proc/cmdline | grep -Fxq "androidboot.serialno=$serial" || exit 6
grep ' /cache ext4 ' /proc/mounts | grep -q 'rw' || exit 7
[ ! -e /cache/recovery/openrecoveryscript ] || { echo 'Existing recovery script; update refused'; exit 8; }

root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
candidate="$root/BOOT_NATIVE_EXPERIMENTAL.img"
boot=/dev/block/platform/155a0000.ufs/by-name/BOOT
recovery=/dev/block/platform/155a0000.ufs/by-name/RECOVERY
[ -b "$boot" ] && [ -b "$recovery" ] || exit 9
[ -f "$candidate" ] && [ ! -L "$candidate" ] || exit 10
[ "$(stat -c %s "$candidate")" = 41943040 ] || exit 11
s7_sha256() { sha256sum "$1" | awk '{print $1}'; }
[ "$(s7_sha256 "$candidate")" = "$candidate_sha" ] || exit 12
[ "$(s7_sha256 "$recovery")" = 919db5c301975559db12cfc04261a03116fd242f525e2c9287257ab9b80d25a6 ] || exit 13
[ "$(s7_sha256 "$boot")" = "$previous_sha" ] || { echo 'Current BOOT changed; update refused'; exit 14; }

base=/cache/s7-native-updates
[ ! -L "$base" ] || exit 15
backup="$root/BOOT_PREVIOUS.img"
# A host-verified copy staged in TWRP RAM permits updates when CACHE is full.
if [ ! -e "$backup" ]; then
    mkdir -p "$base"
    backup="$base/boot-$previous_sha.img"
fi
if [ -e "$backup" ]; then
    [ -f "$backup" ] && [ ! -L "$backup" ] && [ "$(s7_sha256 "$backup")" = "$previous_sha" ] || exit 16
else
    [ ! -e "$backup.tmp" ] || exit 17
    dd if="$boot" of="$backup.tmp" bs=4194304
    sync
    [ "$(s7_sha256 "$backup.tmp")" = "$previous_sha" ] || exit 18
    mv "$backup.tmp" "$backup"
    sync
fi

write_ok=false
if dd if="$candidate" of="$boot" bs=4194304; then write_ok=true; fi
sync
if [ "$write_ok" != true ] || [ "$(s7_sha256 "$boot")" != "$candidate_sha" ]; then
    dd if="$backup" of="$boot" bs=4194304
    sync
    [ "$(s7_sha256 "$boot")" = "$previous_sha" ] || { echo 'ROLLBACK_READBACK_FAILED'; exit 20; }
    echo 'UPDATE_FAILED_PREVIOUS_BOOT_RESTORED'
    exit 19
fi
echo "FLASH_READBACK_OK=$candidate_sha"
echo "PREVIOUS_BOOT=$backup"
reboot
