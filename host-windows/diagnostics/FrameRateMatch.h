#pragma once
#include <cstdint>
namespace s7probe {
// Two representations only: the exact nominal integer FPS or the exact UVC
// interval published by this firmware. This is not a floating tolerance and
// never accepts NTSC 59.94/29.97 as 60/30.
inline bool exactFrameRate(uint32_t numerator,uint32_t denominator,uint32_t fps) {
    if(!numerator||!denominator||
       (fps!=30&&fps!=60&&fps!=120&&fps!=240))return false;
    if(uint64_t(numerator)==uint64_t(fps)*denominator)return true;
    const uint32_t interval=10000000u/fps;
    return uint64_t(numerator)*interval==uint64_t(10000000u)*denominator;
}
}
