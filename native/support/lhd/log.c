/* lhd's logging calls go to supervised stderr, never to logd. */
#include "abi.h"
static int emit(int priority, const char *tag, const char *fmt, va_list ap) {
    char line[1024];
    int n = snprintf(line, sizeof(line), "lhd[%d] %.48s: ", priority, tag ? tag : "");
    if (n < 0 || n >= (int)sizeof(line)-2) return -1;
    int m = vsnprintf(line+n, sizeof(line)-(size_t)n-1, fmt ? fmt : "", ap);
    if (m < 0) return -1;
    size_t used = (size_t)n + ((size_t)m < sizeof(line)-(size_t)n-2 ? (size_t)m : sizeof(line)-(size_t)n-2);
    line[used++]='\n';
    return s7_write_exact(2, line, used) < 0 ? -1 : (int)used;
}
int __android_log_vprint(int p, const char *t, const char *f, va_list a) { return emit(p,t,f,a); }
int __android_log_print(int p, const char *t, const char *f, ...) {
    va_list a; va_start(a,f); int r=emit(p,t,f,a); va_end(a); return r;
}
int __android_log_buf_print(int b, int p, const char *t, const char *f, ...) {
    (void)b; va_list a; va_start(a,f); int r=emit(p,t,f,a); va_end(a); return r;
}
int __android_log_buf_write(int b, int p, const char *t, const char *s) {
    return __android_log_buf_print(b,p,t,"%s",s ? s : "");
}
