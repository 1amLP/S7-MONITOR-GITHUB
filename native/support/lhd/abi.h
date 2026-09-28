/* Minimal declarations for the supplied AArch64 Bionic ABI, not Android NDK.
 * Only used to build the three narrow lhd adapters. No Binder/HAL stubs. */
#ifndef S7_LHD_ABI_H
#define S7_LHD_ABI_H
#include <stdarg.h>
#include <stddef.h>
#include <stdint.h>
typedef long s7_ssize_t;
struct s7_timespec { long tv_sec, tv_nsec; };
extern int *__errno(void);
#define s7_errno (*__errno())
extern int clock_gettime(int, struct s7_timespec *);
extern int open(const char *, int, ...);
extern int close(int);
extern s7_ssize_t write(int, const void *, size_t);
extern int vsnprintf(char *, size_t, const char *, va_list);
extern int snprintf(char *, size_t, const char *, ...);
#define S7_EINTR 4
#define S7_EIO 5
#define S7_ENOENT 2
#define S7_EINVAL 22
#define S7_ENOSPC 28
#define S7_O_WRONLY 1
#define S7_O_CLOEXEC 02000000
static inline size_t s7_length(const char *s, size_t max) {
    size_t n = 0; if (!s) return 0;
    while (n < max && s[n]) ++n;
    return n;
}
static inline int s7_write_exact(int fd, const char *data, size_t n) {
    /* One sysfs command must not be split into several writes. */
    s7_ssize_t r;
    do { r = write(fd, data, n); } while (r < 0 && s7_errno == S7_EINTR);
    if (r < 0) return -1;
    if ((size_t)r != n) { s7_errno = S7_EIO; return -1; }
    return 0;
}
#endif
