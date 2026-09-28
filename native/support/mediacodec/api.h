/* Stable C MediaCodec ABI, from the Android NDK media reference (API21+).
 * Narrow declarations for the supplied AArch64 Bionic, not a replacement codec.
 * All codec calls are resolved from the ORIGINAL libmediandk.so at run time. */
#ifndef S7_MEDIACODEC_API_H
#define S7_MEDIACODEC_API_H
#include <stdint.h>
#include <stddef.h>
#include <stdbool.h>
typedef struct AMediaCodec AMediaCodec;
typedef struct AMediaFormat AMediaFormat;
typedef struct { int32_t offset, size; int64_t presentationTimeUs; uint32_t flags; } CodecInfo;
_Static_assert(sizeof(CodecInfo)==24, "MediaCodecBufferInfo ABI");
struct CodecAPI {
 AMediaCodec *(*createCodecByName)(const char *);
 int32_t (*configure)(AMediaCodec *,const AMediaFormat *,void *,void *,uint32_t);
 int32_t (*start)(AMediaCodec *);
 int32_t (*stop)(AMediaCodec *);
 int32_t (*deleteCodec)(AMediaCodec *);
 long (*dequeueInputBuffer)(AMediaCodec *,int64_t);
 long (*dequeueOutputBuffer)(AMediaCodec *,CodecInfo *,int64_t);
 uint8_t *(*getInputBuffer)(AMediaCodec *,size_t,size_t *);
 uint8_t *(*getOutputBuffer)(AMediaCodec *,size_t,size_t *);
 int32_t (*queueInputBuffer)(AMediaCodec *,size_t,size_t,size_t,uint64_t,uint32_t);
 int32_t (*releaseOutputBuffer)(AMediaCodec *,size_t,bool);
 AMediaFormat *(*getOutputFormat)(AMediaCodec *);
 AMediaFormat *(*getBufferFormat)(AMediaCodec *,size_t);
 AMediaFormat *(*getInputFormat)(AMediaCodec *);
 int32_t (*setParameters)(AMediaCodec *,const AMediaFormat *);
 AMediaFormat *(*newFormat)(void);
 int32_t (*deleteFormat)(AMediaFormat *);
 void (*setInt32)(AMediaFormat *,const char *,int32_t);
 void (*setString)(AMediaFormat *,const char *,const char *);
 void (*setBuffer)(AMediaFormat *,const char *,const void *,size_t);
 bool (*getInt32)(AMediaFormat *,const char *,int32_t *);
 bool (*getRect)(AMediaFormat *,const char *,int32_t *,int32_t *,int32_t *,int32_t *);
 bool (*getBuffer)(AMediaFormat *,const char *,void **,size_t *);
};
#endif
