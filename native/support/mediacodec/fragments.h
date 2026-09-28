#ifndef S7_MEDIACODEC_FRAGMENTS_H
#define S7_MEDIACODEC_FRAGMENTS_H
#include <stddef.h>
#include <stdint.h>
#include "platform.h"
/* Assemble one AMediaCodec access unit, not a queue of H264 pictures.
 * A partial buffer is never published to USB as an independent frame. */
#define MC_AVC_BOUND (4u*1024u*1024u)
struct mc_fragments { size_t used; uint64_t pts; uint32_t flags,pieces; };
/* Returns -1 invalid, 0 incomplete, 1 complete. Caller clears only AFTER emit.
 * All rejected inputs leave the existing access unit unchanged. */
static inline int mc_fragment_push(struct mc_fragments *s,uint8_t *dst,size_t capacity,
                                  const uint8_t *src,size_t len,uint64_t pts,uint32_t flags) {
 if(!s||!dst||!src||!len||len>MC_AVC_BOUND||s->used>MC_AVC_BOUND-len||
    s->used>capacity||len>capacity-s->used||(flags&~11u)||s->pieces>=64||
    (s->pieces&&(pts!=s->pts||((flags^s->flags)&2u))))return -1;
 memcpy(dst+s->used,src,len);
 if(!s->pieces){s->pts=pts;s->flags=flags&3u;}else s->flags|=flags&1u;
 s->used+=len;s->pieces++;
 return (flags&8u)?0:1;
}
#endif
