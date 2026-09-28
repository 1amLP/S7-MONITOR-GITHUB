#ifndef S7_MEDIACODEC_WIRE_H
#define S7_MEDIACODEC_WIRE_H
#include <stdint.h>
#include <stddef.h>
#define MC_HEADER 64u
#define MC_MAX_PAYLOAD (8u*1024u*1024u)
#define MC_OPEN 1u
#define MC_SUBMIT 2u
#define MC_DRAIN 3u
#define MC_CLOSE 4u
#define MC_IDR 5u
#define MC_DECODER 1u
#define MC_ENCODER 2u
#define MC_RETRY (-11)
#define MC_INVALID (-22)
#define MC_UNSUPPORTED (-95)
#define MC_FAILED (-5)
static inline uint32_t mc_u32(const uint8_t *p) { return (uint32_t)p[0]|(uint32_t)p[1]<<8|(uint32_t)p[2]<<16|(uint32_t)p[3]<<24; }
static inline uint64_t mc_u64(const uint8_t *p) { return mc_u32(p)|(uint64_t)mc_u32(p+4)<<32; }
static inline void mc_put32(uint8_t *p,uint32_t v) { for(unsigned i=0;i<4;i++)p[i]=(uint8_t)(v>>(8*i)); }
static inline void mc_put64(uint8_t *p,uint64_t v) { mc_put32(p,(uint32_t)v);mc_put32(p+4,(uint32_t)(v>>32)); }
static inline int mc_valid_header(const uint8_t *h) {
 return h[0]=='S'&&h[1]=='7'&&h[2]=='M'&&h[3]=='C'&&h[4]==3&&h[5]==0&&h[7]==0&&
        h[6]>=MC_OPEN&&h[6]<=MC_IDR&&mc_u32(h+8)&&!mc_u32(h+12)&&
        mc_u32(h+16)<=MC_MAX_PAYLOAD&&!mc_u32(h+52)&&mc_u32(h+56)<=1u&&!mc_u32(h+60);
}
#endif
