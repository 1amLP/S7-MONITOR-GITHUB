#pragma once
#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <limits>
namespace s7camera {
// Copy only visible NV12 rows, never padding. Stride and allocation height are
// separate. Validate the COMPLETE layout before touching output. No scaling,
// negative NV12 stride, aliasing, or speculative reads past a mapped buffer.
inline bool copyNV12Rows(uint8_t const* input,size_t inputBytes,size_t offset,
                         size_t pitch,unsigned codedHeight,uint8_t* output,
                         size_t outputBytes,unsigned width,unsigned height){
    if(!input||!output||!width||!height||width>4096||height>2304||codedHeight>2304||
       height>codedHeight||pitch<width||pitch>16384||((width|height|codedHeight|pitch)&1)||
       inputBytes>64U*1024U*1024U||outputBytes!=size_t(width)*height*3/2||offset>inputBytes)return false;
    const size_t chroma=pitch*codedHeight;
    const size_t end=chroma+pitch*(height/2-1)+width;
    if(end>inputBytes-offset)return false;
    const auto a=reinterpret_cast<uintptr_t>(input),b=reinterpret_cast<uintptr_t>(output);
    if(inputBytes>std::numeric_limits<uintptr_t>::max()-a||outputBytes>std::numeric_limits<uintptr_t>::max()-b||
       (a<b+outputBytes&&b<a+inputBytes))return false;
    for(unsigned y=0;y<height;++y)
        std::copy_n(input+offset+size_t(y)*pitch,width,output+size_t(y)*width);
    for(unsigned y=0;y<height/2;++y)
        std::copy_n(input+offset+chroma+size_t(y)*pitch,width,output+size_t(width)*height+size_t(y)*width);
    return true;
}
inline bool visibleNV12(uint8_t const* input,size_t inputBytes,unsigned paddedWidth,unsigned paddedHeight,
                        uint8_t* output,size_t outputBytes,unsigned width,unsigned height) {
    if(paddedWidth>4096||paddedHeight>2304||width>paddedWidth||
       inputBytes!=size_t(paddedWidth)*paddedHeight*3/2)return false;
    return copyNV12Rows(input,inputBytes,0,paddedWidth,paddedHeight,output,outputBytes,width,height);
}
}
