#ifndef S7_MEDIACODEC_AVC_LIMITS_H
#define S7_MEDIACODEC_AVC_LIMITS_H
#include <stdint.h>
/* AVC Baseline MaxFS/MaxMBPS/MaxBR, not the V4L2 enum or High-profile BR scale.
 * Android CodecProfileLevel uses bit values. Select by ALL three constraints;
 * QHD30 and high bitrate must not inherit an insufficient default level. */
static inline int32_t mc_avc_level(uint32_t w,uint32_t h,uint32_t fps,uint32_t bitrate) {
 static const struct { int32_t omx; uint32_t fs,mbps,br; } limits[]={
  {0x200,3600,108000,14000000}, {0x400,5120,216000,20000000},
  {0x800,8192,245760,20000000}, {0x1000,8192,245760,50000000},
  {0x2000,8704,522240,50000000}, {0x4000,22080,589824,135000000},
  {0x8000,36864,983040,240000000}
 };
 if(!w||!h||w>4096||h>4096||!fps||fps>240||!bitrate)return 0;
 uint64_t fs=((uint64_t)w+15)/16*(((uint64_t)h+15)/16),mbps=fs*fps;
 for(unsigned i=0;i<sizeof(limits)/sizeof(limits[0]);i++)
  if(fs<=limits[i].fs&&mbps<=limits[i].mbps&&bitrate<=limits[i].br)return limits[i].omx;
 return 0;
}
#endif
