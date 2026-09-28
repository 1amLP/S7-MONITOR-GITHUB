#ifndef S7_MEDIACODEC_LAYOUT_H
#define S7_MEDIACODEC_LAYOUT_H
#include <stdint.h>
#include <stddef.h>
#include "platform.h"
/* Only declared linear OMX19 (I420) and OMX21 (NV12). Flexible, Samsung tiled,
 * opaque handles, 10-bit and protected buffers are NOT interpreted as linear. */
struct mc_layout { int32_t width,height,stride,slice,left,top,right,bottom,color; };
struct mc_copy_plan {
 size_t y, u, v, ystep, cstep, ybytes, yrows, crows, required, allocation;
 int planar;
};
/* Build addresses from the negotiated allocation, not from visible height.
 * required is the last byte actually read. Unused final padding is not needed
 * in BufferInfo.size; encoder writes still require the entire allocation. */
static inline int mc_layout_plan(const struct mc_layout *v,int w,int h,struct mc_copy_plan *p) {
 if(!v||!p||w<=0||h<=0||w>4096||h>4096||(w&1)||(h&1)||
    v->width<=0||v->height<=0||v->width>4096||v->height>4096||
    v->stride<v->width||v->slice<v->height||v->stride>8192||v->slice>8192||
    (v->stride&1)||(v->slice&1)||v->left<0||v->top<0||(v->left&1)||(v->top&1)||
    v->right<v->left||v->bottom<v->top||v->right>=v->width||v->bottom>=v->height||
    v->right-v->left+1!=w||v->bottom-v->top+1!=h||(v->color!=19&&v->color!=21))return -1;
 size_t plane=(size_t)v->stride*v->slice;
 struct mc_copy_plan q={0};
 q.y=(size_t)v->top*v->stride+v->left;q.ystep=v->stride;q.ybytes=w;q.yrows=h;q.crows=h/2;
 q.planar=v->color==19;q.cstep=q.planar?(size_t)v->stride/2:(size_t)v->stride;
 q.u=plane+(size_t)(v->top/2)*q.cstep+(q.planar?(size_t)v->left/2:(size_t)v->left);
 q.v=q.planar?q.u+plane/4:q.u+1;
 q.required=q.planar?q.v+(q.crows-1)*q.cstep+(size_t)w/2:q.u+(q.crows-1)*q.cstep+w;
 q.allocation=plane+plane/2;
 if(q.required>q.allocation||q.y+(q.yrows-1)*q.ystep+q.ybytes>plane)return -1;
 *p=q;return 0;
}
static inline int mc_layout_valid(const struct mc_layout *v,int w,int h,size_t size) {
 struct mc_copy_plan p;return !mc_layout_plan(v,w,h,&p)&&p.required<=size;
}
static inline int mc_pack_nv12(uint8_t *dst,size_t capacity,const uint8_t *src,size_t size,
                              const struct mc_layout *v,int w,int h) {
 struct mc_copy_plan p;
 if(!dst||!src||mc_layout_plan(v,w,h,&p))return -1;
 size_t count=(size_t)w*h;
 if(count>capacity||count/2>capacity-count||p.required>size)return -1;
 for(size_t y=0;y<p.yrows;y++)memcpy(dst+y*w,src+p.y+y*p.ystep,p.ybytes);
 for(size_t y=0;y<p.crows;y++){
  uint8_t *d=dst+count+y*w;
  if(!p.planar)memcpy(d,src+p.u+y*p.cstep,p.ybytes);
  else for(int x=0;x<w/2;x++){d[2*x]=src[p.u+y*p.cstep+x];d[2*x+1]=src[p.v+y*p.cstep+x];}
 }
 return 0;
}
static inline int mc_unpack_nv12(uint8_t *dst,size_t capacity,const uint8_t *src,size_t size,
                                const struct mc_layout *v,int w,int h) {
 struct mc_copy_plan p;
 if(!dst||!src||mc_layout_plan(v,w,h,&p)||v->left||v->top||p.allocation>capacity)return -1;
 size_t count=(size_t)w*h,plane=(size_t)v->stride*v->slice;
 if(size!=count+count/2)return -1;
 /* Clear only padding; every visible byte is overwritten by the camera image.
  * This avoids the old full-frame fill immediately followed by a full copy. */
 for(size_t y=0;y<p.yrows;y++){
  memcpy(dst+y*p.ystep,src+y*w,w);
  memset(dst+y*p.ystep+w,0,p.ystep-w);
 }
 memset(dst+p.yrows*p.ystep,0,plane-p.yrows*p.ystep);
 size_t visible=p.planar?(size_t)w/2:(size_t)w;
 for(unsigned component=0;component<(p.planar?2u:1u);component++){
  size_t base=component?plane+plane/4:plane;
  for(size_t y=0;y<p.crows;y++){
   const uint8_t *s=src+count+y*w;uint8_t *d=dst+base+y*p.cstep;
   if(!p.planar)memcpy(d,s,w);
   else for(size_t x=0;x<visible;x++)d[x]=s[2*x+component];
   memset(d+visible,128,p.cstep-visible);
  }
  size_t extent=p.planar?plane/4:plane/2;
  memset(dst+base+p.crows*p.cstep,128,extent-p.crows*p.cstep);
 }
 return 0;
}
#endif
