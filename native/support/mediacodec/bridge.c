/* Isolated native AMediaCodec worker. fd3 is a command/AU socketpair, fd4
 * is a sealed-size raw NV12 exchange slot. Pixels are not sent over fd3.
 * No APK/JVM, no software codec selection, no shell or service-manager stubs.
 * The original Android media runtime/services MUST be supplied separately.
 * Process isolation bounds the owner's waits if an Android/vendor call hangs. */
#include "api.h"
#include "wire.h"
#include "platform.h"
#include "layout.h"
#include "configuration.h"
#include "avc_limits.h"
#include "fragments.h"
#include "shared_pixels.h"
static struct mc_pixels pixels;
static struct CodecAPI api;
static void *library;
static AMediaCodec *codec;
static struct mc_layout layout;
static uint32_t role,width,height,fps,bitrate,gop;
static int started,have_layout,failed,have_pts;
static uint64_t last_pts;
static uint8_t *payload,*output;
static uint8_t csd[65536];
static size_t csd_size;
static struct mc_fragments fragments;
static int exact(int wr,void *p,size_t n) {
 while(n){long r=wr?write(3,p,n):read(3,p,n);if(r<0&&errno==4)continue;if(r<=0)return -1;p=(uint8_t*)p+r;n-=(size_t)r;}return 0;
}
static int load_api(const char *path) {
 library=dlopen(path,2);if(!library)return MC_UNSUPPORTED;
#define LOAD(member,symbol) do { void *sym=dlsym(library,symbol); if(!sym)return MC_UNSUPPORTED;memcpy(&api.member,&sym,sizeof(sym)); } while(0)
 LOAD(createCodecByName,"AMediaCodec_createCodecByName");LOAD(configure,"AMediaCodec_configure");
 LOAD(start,"AMediaCodec_start");LOAD(stop,"AMediaCodec_stop");LOAD(deleteCodec,"AMediaCodec_delete");
 LOAD(dequeueInputBuffer,"AMediaCodec_dequeueInputBuffer");LOAD(dequeueOutputBuffer,"AMediaCodec_dequeueOutputBuffer");
 LOAD(getInputBuffer,"AMediaCodec_getInputBuffer");LOAD(getOutputBuffer,"AMediaCodec_getOutputBuffer");
 LOAD(queueInputBuffer,"AMediaCodec_queueInputBuffer");LOAD(releaseOutputBuffer,"AMediaCodec_releaseOutputBuffer");
 LOAD(getBufferFormat,"AMediaCodec_getBufferFormat");LOAD(getOutputFormat,"AMediaCodec_getOutputFormat");LOAD(getInputFormat,"AMediaCodec_getInputFormat");LOAD(setParameters,"AMediaCodec_setParameters");
 LOAD(newFormat,"AMediaFormat_new");LOAD(deleteFormat,"AMediaFormat_delete");LOAD(setInt32,"AMediaFormat_setInt32");
 LOAD(setString,"AMediaFormat_setString");LOAD(setBuffer,"AMediaFormat_setBuffer");LOAD(getRect,"AMediaFormat_getRect");LOAD(getInt32,"AMediaFormat_getInt32");LOAD(getBuffer,"AMediaFormat_getBuffer");
#undef LOAD
 return 0;
}
static int format_layout(AMediaFormat *f) {
 struct mc_layout v={0};int crop=0;int32_t l=0,t=0,r=0,b=0;
 if(!f)return MC_FAILED;
 int ok=api.getInt32(f,"width",&v.width)&&api.getInt32(f,"height",&v.height)&&api.getInt32(f,"color-format",&v.color);
 if(!ok||v.width<=0||v.height<=0||v.width>4096||v.height>4096){api.deleteFormat(f);return MC_UNSUPPORTED;}
 if(!api.getInt32(f,"stride",&v.stride))v.stride=v.width;
 if(!api.getInt32(f,"slice-height",&v.slice)||v.slice==0)v.slice=v.height;
 v.right=v.width-1;v.bottom=v.height-1;
 crop+=api.getInt32(f,"crop-left",&v.left);crop+=api.getInt32(f,"crop-top",&v.top);
 crop+=api.getInt32(f,"crop-right",&v.right);crop+=api.getInt32(f,"crop-bottom",&v.bottom);
 int rect=api.getRect(f,"crop",&l,&t,&r,&b);int32_t ll=0,tt=0,rr=0,bb=0;
 int alt=api.getRect(f,"crop-rect",&ll,&tt,&rr,&bb);
 if(rect&&alt&&(l!=ll||t!=tt||r!=rr||b!=bb))ok=0;
 if(!rect&&alt){l=ll;t=tt;r=rr;b=bb;rect=1;}
 if(rect){
  if(crop==4&&(v.left!=l||v.top!=t||v.right!=r||v.bottom!=b))ok=0;
  v.left=l;v.top=t;v.right=r;v.bottom=b;
 }
 api.deleteFormat(f);
 if(!ok||(crop!=0&&crop!=4)||!mc_layout_valid(&v,(int)width,(int)height,MC_MAX_PAYLOAD))return MC_UNSUPPORTED;
 layout=v;have_layout=1;return 0;
}
static int open_codec(const uint8_t *h) {
 if(codec||started||failed)return MC_INVALID;
 uint32_t config_size=mc_u32(h+16),sps=0,pps=0;
 uint32_t request_role=mc_u32(h+20);int trial=(request_role&0x100u)!=0;
 role=request_role&~0x100u;width=mc_u32(h+32);height=mc_u32(h+36);fps=mc_u32(h+40);bitrate=mc_u32(h+44);gop=mc_u32(h+48);
 if((role!=MC_DECODER&&role!=MC_ENCODER))return MC_UNSUPPORTED;
 if(trial){
  if(role!=MC_ENCODER||width!=1280||height!=720||(fps!=120&&fps!=240))return MC_UNSUPPORTED;
 }else if(fps!=30&&fps!=60)return MC_UNSUPPORTED;
 if(role==MC_DECODER){if(width!=1280||height!=720||bitrate||gop||!mc_config_sizes(payload,config_size,&sps,&pps))return MC_INVALID;}
 else if(config_size)return MC_INVALID;
 else if(!((width==1280&&height==720)||(width==1920&&height==1080)||(width==2560&&height==1440&&fps==30))||
         bitrate<1000000||bitrate>60000000||!gop||gop>fps*5||gop%fps)return MC_UNSUPPORTED;
 codec=api.createCodecByName(role==MC_DECODER?"OMX.Exynos.avc.dec":"OMX.Exynos.AVC.Encoder");
 /* Names below are the actual component names from the supplied media_codecs.xml,
  * not library filenames. An unrecognized build requires an explicit update. */
 if(!codec)return MC_UNSUPPORTED;
 AMediaFormat *f=api.newFormat();if(!f)return MC_FAILED;
 api.setString(f,"mime","video/avc");api.setInt32(f,"width",(int32_t)width);api.setInt32(f,"height",(int32_t)height);
 api.setInt32(f,"color-format",21);api.setInt32(f,"frame-rate",(int32_t)fps);api.setInt32(f,"priority",0);
 api.setInt32(f,"max-input-size",role==MC_DECODER?1048576:(int32_t)(width*height*3/2));
 if(role==MC_DECODER){
  api.setBuffer(f,"csd-0",payload+8,sps);api.setBuffer(f,"csd-1",payload+8+sps,pps);
 }
 if(role==MC_ENCODER){
  api.setInt32(f,"bitrate",(int32_t)bitrate);api.setInt32(f,"bitrate-mode",2);api.setInt32(f,"i-frame-interval",(int32_t)(gop/fps));
  api.setInt32(f,"max-bframes",0);api.setInt32(f,"profile",1);
  int32_t level=mc_avc_level(width,height,fps,bitrate);
  if(!level){api.deleteFormat(f);return MC_UNSUPPORTED;}
  api.setInt32(f,"level",level); /* AVC Baseline: no B pictures. */
 }
 int r=api.configure(codec,f,0,0,role==MC_ENCODER?1:0);api.deleteFormat(f);if(r)return r;
 r=api.start(codec);if(r)return r;started=1;
 if(role==MC_ENCODER)return format_layout(api.getInputFormat(codec));
 return 0;
}
static int submit(const uint8_t *h) {
 uint32_t len=mc_u32(h+16);uint64_t pts=mc_u64(h+24);
 if(!started||failed||pts>INT64_MAX||(have_pts&&pts<=last_pts)||!len)return MC_INVALID;
 if(role==MC_DECODER){if(len>1048576||len<4)return MC_INVALID;}
 else if(len!=width*height*3/2||!have_layout)return MC_INVALID;
 long index=api.dequeueInputBuffer(codec,0);if(index==-1)return MC_RETRY;if(index<0)return MC_FAILED;
 size_t size=0;uint8_t *p=api.getInputBuffer(codec,(size_t)index,&size);
 if(!p||size>64u*1024u*1024u)return MC_FAILED;
 size_t used=len;
 if(role==MC_DECODER){if(len>size)return MC_INVALID;memcpy(p,payload,len);}
 else {if(mc_unpack_nv12(p,size,payload,len,&layout,(int)width,(int)height))return MC_UNSUPPORTED;used=(size_t)layout.stride*layout.slice*3/2;}
 int r=api.queueInputBuffer(codec,(size_t)index,0,used,pts,0);
 if(!r){last_pts=pts;have_pts=1;}return r;
}
static int collect_csd(AMediaFormat *f) {
 if(!f)return MC_FAILED;csd_size=0;
 for(unsigned i=0;i<2;i++){
  void *data=0;size_t size=0;
  if(api.getBuffer(f,i?"csd-1":"csd-0",&data,&size)){
   if(!data||size>sizeof(csd)-csd_size){api.deleteFormat(f);return MC_INVALID;}
   memcpy(csd+csd_size,data,size);csd_size+=size;
  }
 }
 api.deleteFormat(f);return 0;
}
static int drain(uint8_t *h) {
 if(!started||failed)return MC_INVALID;
 for(unsigned loop=0;loop<8;loop++){
  if(role==MC_ENCODER&&csd_size){memcpy(output,csd,csd_size);mc_put32(h+16,(uint32_t)csd_size);mc_put32(h+20,2);mc_put64(h+24,0);csd_size=0;return 0;}
  CodecInfo info={0};long index=api.dequeueOutputBuffer(codec,&info,0);
  if(index==-1)return MC_RETRY;
  if(index==-3)continue;
  if(index==-2){if(fragments.pieces)return MC_INVALID;int r=role==MC_DECODER?format_layout(api.getOutputFormat(codec)):collect_csd(api.getOutputFormat(codec));if(r)return r;continue;}
  if(index<0)return MC_FAILED;
  size_t size=0;uint8_t *data=api.getOutputBuffer(codec,(size_t)index,&size);int r=0;
  /* Pinned API34 NDK returns abuf->data(), already at the payload offset.
   * BufferInfo.offset and getOutputBuffer(out_size) are NOT reliable bounds on
   * that pointer before API36. Use BufferInfo.size, never add offset a second time.
   * https://developer.android.com/ndk/reference/group/media */
  (void)size;
  if(info.size<0||info.presentationTimeUs<0||(info.size&&!data)||
     (uint32_t)info.size>64u*1024u*1024u||(info.flags&~11u)||(role==MC_DECODER&&(info.flags&10u)))r=MC_INVALID;
  else if(info.size){
   if(role==MC_DECODER){
    /* Format belongs to THIS buffer; a later format event must not
     * reinterpret an older outstanding frame using new pitch/crop metadata. */
    r=format_layout(api.getBufferFormat(codec,(size_t)index));
    if(!r&&(!have_layout||mc_pack_nv12(output,pixels.size,data,(size_t)info.size,&layout,(int)width,(int)height)))r=MC_UNSUPPORTED;
    if(!r)mc_put32(h+16,width*height*3/2);
   } else {
    int complete=mc_fragment_push(&fragments,output,MC_AVC_BOUND,data,(size_t)info.size,
                                  (uint64_t)info.presentationTimeUs,info.flags);
    if(complete<0)r=MC_INVALID;
    else if(complete){mc_put32(h+16,(uint32_t)fragments.used);info.flags=fragments.flags;fragments=(struct mc_fragments){0};}
   }
   if(mc_u32(h+16)){
    mc_put32(h+20,info.flags);mc_put64(h+24,(uint64_t)info.presentationTimeUs);
    mc_put32(h+32,width);mc_put32(h+36,height);mc_put32(h+52,(info.flags&2)?0:1);
   }
  }
  int released=api.releaseOutputBuffer(codec,(size_t)index,false);
  if(released)return released;if(r)return r;if(mc_u32(h+16))return 0;
 }
 return MC_RETRY;
}
static int stop_codec(void) {
 /* Do not declare resources released after a failing stop/delete. The parent
  * keeps this worker until confirmed process exit; a hung worker is quarantined. */
 if(codec){if(started){int r=api.stop(codec);if(r)return r;started=0;}int r=api.deleteCodec(codec);if(r)return r;codec=0;}
 return 0;
}
static uint32_t required_role;
int s7_codec_main(int argc,char **argv,char **envp) {
 (void)envp;
 const char *path="/system/lib64/libmediandk.so";
#ifdef S7_CODEC_HOST_TEST
 if(argc==2)path=argv[1];
#else
 if(argc!=2 || !argv[1] || argv[1][1] || (argv[1][0]!='1' && argv[1][0]!='2'))return 64;
 required_role=(uint32_t)(argv[1][0]-'0');
#endif
 if(mc_pixels_open(&pixels,required_role))return 72;
 required_role=pixels.role;
 int loaded=load_api(path);uint32_t previous=0;
 payload=required_role==MC_DECODER?malloc(1048576u):pixels.data;
 output=required_role==MC_DECODER?pixels.data:malloc(4u*1024u*1024u);
 if(!payload||!output){if(required_role==MC_DECODER)free(payload);else free(output);mc_pixels_close(&pixels);return 70;}
 for(;;){
  uint8_t h[MC_HEADER],response[MC_HEADER];
  if(exact(0,h,sizeof(h)))break;
  if(!mc_valid_header(h)||mc_u32(h+8)!=previous+1)break;
  previous=mc_u32(h+8);uint32_t op=h[6],len=mc_u32(h+16),storage=mc_u32(h+56);
  if(op!=MC_SUBMIT&&op!=MC_OPEN&&len)break;
  if(op==MC_OPEN&&len>65536)break;
  if(storage&&(op!=MC_SUBMIT||required_role!=MC_ENCODER||!len||len>pixels.size))break;
  if(op==MC_SUBMIT&&required_role==MC_ENCODER&&!storage)break;
  if(!storage&&len){if(len>1048576u||required_role!=MC_DECODER||exact(0,payload,len))break;}
  memcpy(response,h,sizeof(h));response[7]=0x80;mc_put32(response+16,0);mc_put32(response+20,0);mc_put32(response+52,0);mc_put32(response+56,0);
  int r=loaded;
  if(!r){
   if(op==MC_OPEN)r=(required_role && (mc_u32(h+20)&~0x100u)!=required_role)?MC_INVALID:open_codec(h);
   else if(op==MC_CLOSE)r=stop_codec();
   else if(!codec||!started||failed)r=MC_FAILED;
   else if(op==MC_SUBMIT)r=submit(h);
   else if(op==MC_DRAIN)r=drain(response);
   else if(op==MC_IDR){
    if(role!=MC_ENCODER)r=MC_INVALID;
    else {AMediaFormat *f=api.newFormat();if(!f)r=MC_FAILED;else {api.setInt32(f,"request-sync",0);r=api.setParameters(codec,f);api.deleteFormat(f);}}
   }
  }
  if(r&&r!=MC_RETRY){failed=1;mc_put32(response+16,0);mc_put32(response+20,0);mc_put32(response+52,0);}
  mc_put32(response+12,(uint32_t)r);
  if(!r&&required_role==MC_DECODER&&mc_u32(response+16))mc_put32(response+56,MC_PIXELS);
  /* Sending the response transfers slot ownership back to the caller. The
   * worker cannot touch it again until the NEXT socket command arrives. */
  if(exact(1,response,sizeof(response))||(!mc_u32(response+56)&&exact(1,output,mc_u32(response+16))))break;
  if(op==MC_CLOSE)break;
 }
 int result=stop_codec();
 if(required_role==MC_DECODER)free(payload);else free(output);
 if(mc_pixels_close(&pixels))result=MC_FAILED;
 if(library)dlclose(library);close(3);return result?71:0;
}
#ifdef S7_CODEC_HOST_TEST
int main(int argc,char **argv,char **envp){return s7_codec_main(argc,argv,envp);}
#endif
