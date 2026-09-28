#ifndef S7_SHARED_PIXELS_H
#define S7_SHARED_PIXELS_H
#include "platform.h"
/* Linux3.18 fixed memfd ABI, shared with internal/codecmem. No executable map,
 * DMA pointer, persistent path or resize after either process receives fd4. */
#define MC_DECODE_PIXELS (1280u*720u*3u/2u)
#define MC_ENCODE_PIXELS (2560u*1440u*3u/2u)
#define MC_PIXEL_FD 4
#define MC_PIXELS 1u
struct mc_pixels { uint8_t *data; size_t size; uint32_t role; };
static int mc_pixels_open(struct mc_pixels *p,uint32_t expected) {
 long seals=fcntl(MC_PIXEL_FD,1034);
 long bytes=lseek(MC_PIXEL_FD,0,2);
 uint32_t r=bytes==(long)MC_DECODE_PIXELS?1u:(bytes==(long)MC_ENCODE_PIXELS?2u:0u);
 if(seals!=7||!r||(expected&&expected!=r))return -1;
 /* Decoder writes; encoder only reads. PROT_EXEC is never requested. */
 void *m=mmap(0,(size_t)bytes,r==1?3:1,1,MC_PIXEL_FD,0);
 if(m==(void*)-1)return -1;
 *p=(struct mc_pixels){(uint8_t*)m,(size_t)bytes,r};return 0;
}
static int mc_pixels_close(struct mc_pixels *p) {
 int result=p->data?munmap(p->data,p->size):0;
 *p=(struct mc_pixels){0};close(MC_PIXEL_FD);return result;
}
#endif
