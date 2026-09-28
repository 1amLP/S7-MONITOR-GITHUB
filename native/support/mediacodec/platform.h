#ifndef S7_MEDIACODEC_PLATFORM_H
#define S7_MEDIACODEC_PLATFORM_H
#include <stddef.h>
#include <stdint.h>
#ifdef S7_CODEC_HOST_TEST
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <sys/mman.h>
#else
extern int *__errno(void);
#define errno (*__errno())
extern long read(int,void *,size_t);
extern long write(int,const void *,size_t);
extern int close(int);
extern long lseek(int,long,int);
extern int fcntl(int,int,...);
extern void *mmap(void *,size_t,int,int,int,long);
extern int munmap(void *,size_t);
extern void *malloc(size_t);
extern void free(void *);
extern void *memcpy(void *,const void *,size_t);
extern void *memset(void *,int,size_t);
extern void *dlopen(const char *,int);
extern void *dlsym(void *,const char *);
extern int dlclose(void *);
extern const char *dlerror(void);
#endif
#endif
