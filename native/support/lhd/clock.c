/* Exact int64_t elapsedRealtime() ABI used by lhd; milliseconds since boot,
 * including suspend. No generated timestamps and no service dependency. */
#include "abi.h"
int64_t s7_elapsedRealtime(void) __asm__("_ZN7android15elapsedRealtimeEv");
int64_t s7_elapsedRealtime(void) {
    struct s7_timespec t;
    if (clock_gettime(7 /* CLOCK_BOOTTIME */, &t) != 0) return -1;
    return (int64_t)t.tv_sec * 1000 + t.tv_nsec / 1000000;
}
