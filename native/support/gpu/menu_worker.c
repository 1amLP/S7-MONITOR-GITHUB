/* Fixed-size native Mali menu worker. FD 3 is the sole output DMA-BUF.
 * stdin carries only draw commands and the immutable glyph atlas. */
typedef unsigned long size_t;
typedef unsigned int u32;
typedef int i32;
typedef void *object;
extern long read(int, void *, size_t);
extern long write(int, const void *, size_t);
extern int *__errno(void);
extern void *malloc(size_t);
extern void free(void *);
extern void *memset(void *, int, size_t);
extern void *dlopen(const char *, int);
extern void *dlsym(void *, const char *);
extern char *strstr(const char *, const char *);
extern unsigned int alarm(unsigned int);
extern int close(int);
struct io_vector { void *base; size_t bytes; };
struct socket_message { void *name; u32 name_bytes,pad; struct io_vector *vectors; size_t vector_count; void *control; size_t control_bytes; i32 flags,pad2; };
struct control_header { size_t bytes; i32 level,type; };
extern long recvmsg(int,struct socket_message *,int);
_Static_assert(sizeof(struct socket_message)==56,"Bionic LP64 msghdr");
_Static_assert(sizeof(struct control_header)==16,"Bionic LP64 cmsghdr");
struct timestamp { long seconds, nanos; };
extern int clock_gettime(int, struct timestamp *);

#include "menu_kernel.inc"

#define MAX_COMMANDS 4096u
#define MAX_TILES 3600u
#define MAX_INDICES 1048576u
#define OUTPUT_BYTES (1440u * 2560u * 4u)
#define BLUR_BYTES (1u<<20)
#define PREVIEW_OUTPUT_BYTES (4u<<20)

typedef i32 (*platforms_fn)(u32,object *,u32 *);
typedef i32 (*devices_fn)(object,unsigned long,u32,object *,u32 *);
typedef i32 (*info_fn)(object,u32,size_t,void *,size_t *);
typedef object (*context_fn)(const long *,u32,const object *,void *,void *,i32 *);
typedef object (*queue_fn)(object,object,unsigned long,i32 *);
typedef object (*import_fn)(object,unsigned long,const long *,void *,size_t,i32 *);
typedef object (*buffer_fn)(object,unsigned long,size_t,void *,i32 *);
typedef object (*program_fn)(object,u32,const char **,const size_t *,i32 *);
typedef i32 (*build_fn)(object,u32,const object *,const char *,void *,void *);
typedef i32 (*build_info_fn)(object,object,u32,size_t,void *,size_t *);
typedef object (*kernel_fn)(object,const char *,i32 *);
typedef i32 (*arg_fn)(object,u32,size_t,const void *);
typedef i32 (*write_fn)(object,object,u32,size_t,size_t,const void *,u32,const object *,object *);
typedef i32 (*enqueue_fn)(object,object,u32,const size_t *,const size_t *,const size_t *,u32,const object *,object *);
typedef i32 (*finish_fn)(object);
typedef i32 (*release_fn)(object);

static object library, context, queue, kernel, program, output, glyphs, commandBuffer, offsetBuffer, indexBuffer;
static object previewKernel,previewOutput[2],composeKernel,menuOther;
static u32 menuSlot;
static import_fn import_dma;
struct dma_image {object planes[2];int fds[2];u32 bytes[2],count,writable;};
static struct dma_image camera_images[8];
static int preview_only;
static arg_fn set_arg;
static write_fn upload;
static enqueue_fn enqueue;
static finish_fn finish;
static release_fn release_mem, release_kernel, release_program, release_queue, release_context;

static int transfer(int fd, void *bytes, size_t count, int writing) {
    unsigned char *p = bytes;
    while (count) {
        long n = writing ? write(fd,p,count) : read(fd,p,count);
        if (n < 0 && *__errno() == 4) continue;
        if (n <= 0 || (size_t)n > count) return -1;
        p += n; count -= (size_t)n;
    }
    return 0;
}
static void reply(u32 sequence, i32 status, u32 elapsed) {
    u32 header[4] = {0x31524d47u,sequence,(u32)status,elapsed};
    (void)transfer(1,header,sizeof(header),1);
}
static unsigned long micros(void) {
    struct timestamp t;
    if (clock_gettime(1,&t)) return 0;
    return (unsigned long)t.seconds*1000000ul+(unsigned long)t.nanos/1000ul;
}

static void release_image(struct dma_image *im) {
    for(u32 i=0;i<im->count;i++){if(im->planes[i])(void)release_mem(im->planes[i]);if(im->fds[i]>=0)(void)close(im->fds[i]);}
    (void)memset(im,0,sizeof(*im));
}
static int receive_image(u32 slot,u32 count,u32 ybytes,u32 uvbytes,u32 writable) {
    if(slot>=8||count<1||count>2||!ybytes||ybytes>(32u<<20)||(count==2&&(!uvbytes||uvbytes>(32u<<20)))||writable>1||(preview_only&&writable))return -1030;
    unsigned char marker=0;
    struct io_vector vector={&marker,1};
    union {unsigned long align;unsigned char bytes[32];} control={0};
    struct socket_message msg={0,0,0,&vector,1,control.bytes,sizeof(control),0,0};
    long n=recvmsg(8,&msg,0x40000000);
    struct control_header *head=(struct control_header *)control.bytes;
    int *fds=(int *)(control.bytes+sizeof(*head));
    if(n!=1||marker!='D'||(msg.flags&40)||head->level!=1||head->type!=1||head->bytes!=sizeof(*head)+count*4){
        if(n>=0&&head->level==1&&head->type==1&&head->bytes>=sizeof(*head))for(u32 i=0;i<2&&sizeof(*head)+(i+1)*4<=head->bytes;i++)(void)close(fds[i]);
        return -1031;
    }
    struct dma_image incoming={0};incoming.count=count;incoming.bytes[0]=ybytes;incoming.bytes[1]=uvbytes;incoming.writable=writable;
    for(u32 i=0;i<count;i++)incoming.fds[i]=fds[i];
    const long properties[]={0x40b2,0x40b4,0};i32 error=0;
    for(u32 i=0;i<count;i++){
        incoming.planes[i]=import_dma(context,writable?2:4,properties,&incoming.fds[i],incoming.bytes[i],&error);
        if(!incoming.planes[i]||error){release_image(&incoming);return error?error:-1032;}
    }
    release_image(&camera_images[slot]);camera_images[slot]=incoming;
    return 0;
}

static i32 initialize(void *atlas, size_t atlasBytes) {
    library=dlopen("libGLES_mali.so",2);
    if (!library) return -1001;
#define LOAD(type,name,symbol) type name=(type)dlsym(library,symbol); if (!name) return -1002
    LOAD(platforms_fn,platforms,"clGetPlatformIDs");
    LOAD(devices_fn,devices,"clGetDeviceIDs");
    LOAD(info_fn,info,"clGetDeviceInfo");
    LOAD(context_fn,create_context,"clCreateContext");
    LOAD(queue_fn,create_queue,"clCreateCommandQueue");
    LOAD(import_fn,import_memory,"clImportMemoryARM");
    import_dma=import_memory;
    LOAD(buffer_fn,create_buffer,"clCreateBuffer");
    LOAD(program_fn,create_program,"clCreateProgramWithSource");
    LOAD(build_fn,build,"clBuildProgram");
    LOAD(build_info_fn,build_info,"clGetProgramBuildInfo");
    LOAD(kernel_fn,create_kernel,"clCreateKernel");
#undef LOAD
#define ASSIGN(type,name,symbol) name=(type)dlsym(library,symbol); if (!name) return -1002
    ASSIGN(arg_fn,set_arg,"clSetKernelArg");
    ASSIGN(write_fn,upload,"clEnqueueWriteBuffer");
    ASSIGN(enqueue_fn,enqueue,"clEnqueueNDRangeKernel");
    ASSIGN(finish_fn,finish,"clFinish");
    ASSIGN(release_fn,release_mem,"clReleaseMemObject");
    ASSIGN(release_fn,release_kernel,"clReleaseKernel");
    ASSIGN(release_fn,release_program,"clReleaseProgram");
    ASSIGN(release_fn,release_queue,"clReleaseCommandQueue");
    ASSIGN(release_fn,release_context,"clReleaseContext");
#undef ASSIGN
    object platformList[16], device=0;u32 count=0;
    i32 error=platforms(16,platformList,&count);
    if (error || count>16) return error?error:-1003;
    for (u32 i=0;i<count;i++) {
        u32 deviceCount=0;
        if (!devices(platformList[i],4,1,&device,&deviceCount) && deviceCount) break;
        device=0;
    }
    if (!device) return -1004;
    char extensions[8192]={0};
    if (info(device,0x1030,sizeof(extensions),extensions,0) || !strstr(extensions,"cl_arm_import_memory_dma_buf")) return -1005;
    context=create_context(0,1,&device,0,0,&error);if (!context||error)return error?error:-1006;
    queue=create_queue(context,device,0,&error);if (!queue||error)return error?error:-1007;
    const long properties[]={0x40b2,0x40b4,0};int fd=3;
    if(preview_only){
        for(int i=0;i<2;i++){fd=6+i;previewOutput[i]=import_memory(context,1,properties,&fd,PREVIEW_OUTPUT_BYTES,&error);if(!previewOutput[i]||error)return error?error:-1022;}
    }else{
        output=import_memory(context,1,properties,&fd,OUTPUT_BYTES,&error);if(!output||error)return error?error:-1008;
        fd=4;menuOther=import_memory(context,1,properties,&fd,OUTPUT_BYTES,&error);if(!menuOther||error)return error?error:-1034;
        glyphs=create_buffer(context,4|32,atlasBytes,atlas,&error);if(!glyphs||error)return error?error:-1009;
        commandBuffer=create_buffer(context,4,MAX_COMMANDS*64,0,&error);if(!commandBuffer||error)return error?error:-1010;
        offsetBuffer=create_buffer(context,4,(MAX_TILES+1)*4,0,&error);if(!offsetBuffer||error)return error?error:-1011;
        indexBuffer=create_buffer(context,4,MAX_INDICES*4,0,&error);if(!indexBuffer||error)return error?error:-1012;
    }
    const char *source=menu_kernel;
    program=create_program(context,1,&source,0,&error);if(!program||error)return error?error:-1013;
    error=build(program,1,&device,"-cl-std=CL1.2",0,0);
    if(error){char log[2048]={0};if(!build_info(program,device,0x1183,sizeof(log),log,0))(void)write(2,log,sizeof(log));return error;}
    if(preview_only){previewKernel=create_kernel(program,"preview_rgb",&error);return (!previewKernel||error)?(error?error:-1024):0;}
    kernel=create_kernel(program,"raster",&error);if(!kernel||error)return error?error:-1014;
    composeKernel=create_kernel(program,"compose_rgb",&error);if(!composeKernel||error)return error?error:-1033;
    object args[]={output,glyphs,commandBuffer,offsetBuffer,indexBuffer};
    for(u32 i=0;i<5;i++){error=set_arg(kernel,i,sizeof(object),&args[i]);if(error)return error;}
    return 0;
}

static void cleanup(void) {
    if(menuOther&&release_mem)(void)release_mem(menuOther);
    if(composeKernel&&release_kernel)(void)release_kernel(composeKernel);
    if(previewKernel&&release_kernel)(void)release_kernel(previewKernel);
    if(kernel&&release_kernel)(void)release_kernel(kernel);
    if(program&&release_program)(void)release_program(program);
    if(release_mem){for(int i=0;i<8;i++)release_image(&camera_images[i]);object objects[]={previewOutput[0],previewOutput[1],indexBuffer,offsetBuffer,commandBuffer,glyphs,output};for(u32 i=0;i<7;i++)if(objects[i])(void)release_mem(objects[i]);}
    if(queue&&release_queue)(void)release_queue(queue);
    if(context&&release_context)(void)release_context(context);
}

static int validate(i32 *commands,u32 count,int width,int height,u32 atlasBytes) {
    for(u32 i=0;i<count;i++){
        i32 *c=commands+i*16;
        if(c[0]<1||c[0]>3||c[1]<0||c[2]<0||c[3]>width||c[4]>height||c[1]>=c[3]||c[2]>=c[4]||c[5]<0||c[5]>0xffffff||c[6]<0||c[6]>255)return -1015;
        for(int j=7;j<=10;j++)if(c[j]<-8192||c[j]>8192)return -1015;
        if(c[0]==2){if(c[9]<=0||c[10]<=0||c[11]<0||(unsigned long)c[11]+(unsigned long)c[9]*(unsigned long)c[10]>atlasBytes)return -1015;}
        else if(c[11]<0||c[11]>4096)return -1015;
    }
    return 0;
}

int s7_menu_main(int argc,char **argv,char **envp) {
    (void)argc;(void)argv;(void)envp;
    u32 init[4];if(transfer(0,init,sizeof(init),0))return 2;
    preview_only=init[0]==0x32414d47u;
    if((!preview_only&&init[0]!=0x31414d47u)||(!preview_only&&!init[1])||init[1]>(8u<<20)||init[2]!=1440||init[3]!=2560)return 2;
    void *atlas=init[1]?malloc(init[1]):0;if(init[1]&&(!atlas||transfer(0,atlas,init[1],0))){free(atlas);return 3;}
    (void)alarm(10);i32 error=initialize(atlas,init[1]);(void)alarm(0);free(atlas);reply(0,error,0);
    if(error){cleanup();return 4;}
    i32 *commands=0;u32 *offsets=0,*cursor=0,*indices=0;
    if(!preview_only){
        commands=malloc(MAX_COMMANDS*64);offsets=malloc((MAX_TILES+1)*4);cursor=malloc(MAX_TILES*4);indices=malloc(MAX_INDICES*4);
        if(!commands||!offsets||!cursor||!indices){free(commands);free(offsets);free(cursor);free(indices);cleanup();return 5;}
    }
    for(;;){
        u32 h[8];if(transfer(0,h,sizeof(h),0))break;
        u32 rotation=h[2],count=h[3];int width=(int)h[4],height=(int)h[5];
        if(h[0]==0x31494d47u){if(!h[1]||h[7])break;(void)alarm(2);error=receive_image(h[2],h[3],h[4],h[5],h[6]);(void)alarm(0);reply(h[1],error,0);if(error)break;continue;}
        if(h[0]==0x31434d47u){
            if(preview_only||!h[1]||h[2]>=8||h[3]>=8||h[2]==h[3]||h[4]!=1440||h[5]!=2560||h[6]||h[7])break;
            struct dma_image *src=&camera_images[h[2]],*dst=&camera_images[h[3]];
            if(src->count!=1||dst->count!=1||src->writable||!dst->writable||src->bytes[0]!=OUTPUT_BYTES||dst->bytes[0]!=OUTPUT_BYTES)break;
            (void)alarm(2);unsigned long began=micros();
            object buffers[]={src->planes[0],menuSlot?menuOther:output,dst->planes[0]};error=0;
            for(u32 i=0;!error&&i<3;i++)error=set_arg(composeKernel,i,sizeof(object),&buffers[i]);
            size_t global[]={1440u*2560u};
            if(!error)error=enqueue(queue,composeKernel,1,0,global,0,0,0,0);
            if(!error)error=finish(queue);
            (void)alarm(0);reply(h[1],error,(u32)(micros()-began));if(error)break;
            continue;
        }
        if(h[0]==0x32504d47u){
            if(!preview_only)break;
            int panelRotation=(int)(rotation>>16);rotation&=0xffffu;
            int cw=(int)h[6],ch=(int)h[7];
            if(!h[1]||(rotation!=0&&rotation!=90&&rotation!=180&&rotation!=270)||panelRotation>270||panelRotation%90||count>3||width<2||height<2||width>2560||height>1440||((width|height|cw|ch)&1)||cw<2||ch<2||cw>width||ch>height)break;
            u32 p[8];if(transfer(0,p,sizeof(p),0))break;
            if(p[0]>=8||!camera_images[p[0]].count||p[1]<(u32)width||p[2]<(u32)width||p[1]>32768||p[2]>32768||p[5]>1||p[6]<2||p[7]<2||p[6]>1024||p[7]>1024||((p[6]|p[7])&1))break;
            struct dma_image *im=&camera_images[p[0]];u32 uvplane=im->count-1;
            if((unsigned long)p[3]+(unsigned long)p[1]*(height-1)+(u32)width>im->bytes[0]||(unsigned long)p[4]+(unsigned long)p[2]*(height/2-1)+(u32)width>im->bytes[uvplane])break;
            (void)alarm(2);unsigned long began=micros();
            object buffers[]={im->planes[0],im->planes[uvplane],previewOutput[count>>1]};error=0;
            for(u32 i=0;!error&&i<3;i++)error=set_arg(previewKernel,i,sizeof(object),&buffers[i]);
            int shape[]={width,height,cw,ch,(int)rotation,(int)(count&1),panelRotation,(int)p[1],(int)p[2],(int)p[3],(int)p[4],(int)p[5],(int)p[6],(int)p[7]};
            for(u32 i=0;!error&&i<14;i++)error=set_arg(previewKernel,3+i,sizeof(int),&shape[i]);
            size_t global[]={panelRotation%180?p[7]:p[6],panelRotation%180?p[6]:p[7]};
            if(!error)error=enqueue(queue,previewKernel,2,0,global,0,0,0,0);
            if(!error)error=finish(queue);
            (void)alarm(0);reply(h[1],error,(u32)(micros()-began));if(error)break;
            continue;
        }
        if(h[0]==0x31424d47u){
            break; /* Blur removed: no hidden camera/menu GPU queue dependency. */
        }
        int swapped=rotation==90||rotation==270;
        if(preview_only||h[0]!=0x31444d47u||!h[1]||!count||count>MAX_COMMANDS||
            (rotation!=0&&rotation!=90&&rotation!=180&&rotation!=270)||width!=(swapped?2560:1440)||height!=(swapped?1440:2560)||h[6]>1||h[7])break;
        if(transfer(0,commands,count*64,0))break;
        error=validate(commands,count,width,height,init[1]);if(error){reply(h[1],error,0);break;}
        int columns=(width+31)/32,rows=(height+31)/32;u32 tiles=(u32)(columns*rows);
        if(tiles>MAX_TILES){reply(h[1],-1016,0);break;}
        (void)memset(offsets,0,(tiles+1)*4);
        for(u32 i=0;i<count;i++){
            i32 *c=commands+i*16;
            for(int y=c[2]/32;y<=(c[4]-1)/32;y++)for(int x=c[1]/32;x<=(c[3]-1)/32;x++)offsets[y*columns+x+1]++;
        }
        for(u32 i=1;i<=tiles;i++)offsets[i]+=offsets[i-1];
        if(offsets[tiles]>MAX_INDICES){reply(h[1],-1017,0);break;}
        for(u32 i=0;i<tiles;i++)cursor[i]=offsets[i];
        for(u32 i=0;i<count;i++){
            i32 *c=commands+i*16;
            for(int y=c[2]/32;y<=(c[4]-1)/32;y++)for(int x=c[1]/32;x<=(c[3]-1)/32;x++)indices[cursor[y*columns+x]++]=i;
        }
        (void)alarm(2);unsigned long began=micros();
        object renderTarget=h[6]?menuOther:output;
        error=set_arg(kernel,0,sizeof(object),&renderTarget);
        if(!error)error=upload(queue,commandBuffer,1,0,count*64,commands,0,0,0);
        if(!error)error=upload(queue,offsetBuffer,1,0,(tiles+1)*4,offsets,0,0,0);
        if(!error)error=upload(queue,indexBuffer,1,0,offsets[tiles]*4,indices,0,0,0);
        int shape[]={width,height,(int)rotation,columns};
        for(u32 i=0;!error&&i<4;i++)error=set_arg(kernel,5+i,sizeof(int),&shape[i]);
        size_t global[]={1440,2560};
        if(!error)error=enqueue(queue,kernel,2,0,global,0,0,0,0);
        if(!error)error=finish(queue);
        if(!error)menuSlot=h[6];
        (void)alarm(0);reply(h[1],error,(u32)(micros()-began));if(error)break;
    }
    free(commands);free(offsets);free(cursor);free(indices);cleanup();return 0;
}
