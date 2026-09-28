/* Real kernel wake-lock adapter. No libhardware_legacy/Binder suspend service.
 * Collapse lhd's named locks into one private kernel lock, preserving named-set
 * (not recursive-count) semantics. Supervisor releases it after confirmed exit. */
#include "abi.h"
#ifndef S7_WAKE_ROOT
#define S7_WAKE_ROOT "/sys/power"
#endif
static char names[16][64];
static unsigned char mutex;
static void lock(void) { while (__atomic_test_and_set(&mutex,__ATOMIC_ACQUIRE)) {} }
static void unlock(void) { __atomic_clear(&mutex,__ATOMIC_RELEASE); }
static int same(const char *a, const char *b) {
    for (size_t i=0;i<64;i++) { if(a[i]!=b[i])return 0; if(!a[i])return 1; } return 0;
}
static int valid(const char *n) {
    size_t l=s7_length(n,64); if(!l || l==64)return 0;
    for(size_t i=0;i<l;i++)if((unsigned char)n[i]<=32 || (unsigned char)n[i]>126)return 0;
    return 1;
}
static int command(int acquire) {
    int fd=open(acquire ? S7_WAKE_ROOT "/wake_lock" : S7_WAKE_ROOT "/wake_unlock", S7_O_WRONLY|S7_O_CLOEXEC);
    if(fd<0)return -1;
    const char text[]="s7_native_lhd";
    int r=s7_write_exact(fd,text,sizeof(text)-1), saved=s7_errno;
    (void)close(fd); /* A close error cannot undo a successful sysfs write. */
    if(r<0)s7_errno=saved;
    return r;
}
int acquire_wake_lock(int level, const char *name) {
    if(level!=1 || !valid(name)){s7_errno=S7_EINVAL;return -1;}
    lock(); int empty=-1,count=0;
    for(int i=0;i<16;i++){
        if(names[i][0]){++count;if(same(names[i],name)){unlock();return 0;}}
        else if(empty<0)empty=i;
    }
    if(empty<0){unlock();s7_errno=S7_ENOSPC;return -1;}
    if(!count && command(1)<0){unlock();return -1;}
    size_t n=s7_length(name,63);for(size_t j=0;j<n;j++)names[empty][j]=name[j];names[empty][n]=0;
    unlock();return 0;
}
int release_wake_lock(const char *name) {
    if(!valid(name)){s7_errno=S7_EINVAL;return -1;}
    lock();int found=-1,count=0;
    for(int i=0;i<16;i++)if(names[i][0]){++count;if(same(names[i],name))found=i;}
    if(found<0){unlock();s7_errno=S7_ENOENT;return -1;}
    if(count==1 && command(0)<0){unlock();return -1;}
    names[found][0]=0;unlock();return 0;
}
