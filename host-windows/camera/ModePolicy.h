#pragma once
#include <cstdint>
namespace s7camera {
struct WebcamMode {
    uint32_t width,height,fps;
    bool operator==(WebcamMode const& r)const{return width==r.width&&height==r.height&&fps==r.fps;}
};
struct VisibleVideoFormat {uint32_t width,height,rate,denominator;};
inline bool sameVisibleVideoFormat(VisibleVideoFormat actual,VisibleVideoFormat selected){
    return actual.width==selected.width&&actual.height==selected.height&&actual.rate&&actual.denominator&&
           selected.rate&&selected.denominator&&uint64_t(actual.rate)*selected.denominator==uint64_t(selected.rate)*actual.denominator;
}
inline bool knownMode(WebcamMode m){
 return (m.width==1280&&m.height==720&&(m.fps==30||m.fps==60||m.fps==120||m.fps==240)) ||
        (m.width==1920&&m.height==1080&&(m.fps==30||m.fps==60)) ||
        (m.width==2560&&m.height==1440&&m.fps==30);
}
inline bool webcamEligible(WebcamMode m){ return knownMode(m); }
// The phone selects the native mode. A virtual endpoint must not advertise an
// upscaled picture or repeated frames as a different native camera capability.
inline bool nativeOutput(WebcamMode native,uint32_t w,uint32_t h,uint32_t n,uint32_t d){
    return webcamEligible(native)&&d&&w==native.width&&h==native.height&&uint64_t(native.fps)*d==n;
}
inline uint32_t nv12Bytes(WebcamMode m){return knownMode(m)?m.width*m.height*3/2:0;}
}
