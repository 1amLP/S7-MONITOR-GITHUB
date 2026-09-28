#ifndef S7_MEDIACODEC_CONFIGURATION_H
#define S7_MEDIACODEC_CONFIGURATION_H
#include "wire.h"
/* A single Annex-B SPS and PPS, length delimited for the local C/Go protocol.
 * No trusting an integer from the client before checking every span. */
static inline int mc_parameter_set(const uint8_t *p,size_t n,unsigned type) {
 if(n<5||p[0]||p[1]||p[2]||p[3]!=1||(p[4]&0x80)||(p[4]&31)!=type)return 0;
 for(size_t i=5;i+2<n;i++)if(p[i]==0&&p[i+1]==0&&p[i+2]==1)return 0;
 return 1;
}
static inline int mc_config_sizes(const uint8_t *p,size_t n,uint32_t *sps,uint32_t *pps) {
 if(n<18||n>65536)return 0;
 *sps=mc_u32(p);*pps=mc_u32(p+4);
 if(*sps>n-8||*pps!=n-8-*sps)return 0;
 return mc_parameter_set(p+8,*sps,7)&&mc_parameter_set(p+8+*sps,*pps,8);
}
#endif
