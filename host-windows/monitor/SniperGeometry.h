#pragma once
#include <algorithm>
#include <cstdint>
#include <cmath>

namespace s7 {
struct SniperCrop {
    int32_t left,top,width,height;
};

// One crop drives both GPU sampling and absolute input. Axis scales multiply
// magnification independently; 100% leaves the selected base aspect unchanged.
constexpr SniperCrop sniperCrop(int32_t w,int32_t h,uint32_t zoom,uint32_t scaleX,uint32_t scaleY,uint32_t x,uint32_t y,bool stretch){
    if(w<=0||h<=0||zoom<100||zoom>1600||scaleX<25||scaleX>400||scaleY<25||scaleY>400||x>10000||y>10000)return {};
    const int64_t baseW=stretch?std::min(w,h):std::min<int64_t>(w,int64_t(h)*16/9);
    const int64_t baseH=stretch?std::min(w,h):std::min<int64_t>(h,int64_t(w)*9/16);
    const auto width=int32_t(std::clamp<int64_t>(baseW*10000/(uint64_t(zoom)*scaleX),1,w));
    const auto height=int32_t(std::clamp<int64_t>(baseH*10000/(uint64_t(zoom)*scaleY),1,h));
    const auto left=std::clamp<int32_t>(int32_t(int64_t(w)*x/10000)-width/2,0,w-width);
    const auto top=std::clamp<int32_t>(int32_t(int64_t(h)*y/10000)-height/2,0,h-height);
    return {left,top,width,height};
}

struct SniperTransform {
    float x[4]{},y[4]{};
    bool map(float u,float v,float& sx,float& sy)const {
        sx=x[0]*u+x[1]*v+x[2];sy=y[0]*u+y[1]*v+y[2];
        return sx>=0&&sx<=1&&sy>=0&&sy<=1;
    }
};

// The shader and injected pointer use these same inverse-canvas rows.
inline SniperTransform sniperTransform(SniperCrop crop,int32_t w,int32_t h,uint32_t rotation,bool mirror){
    SniperTransform t;
    if(w<2||h<2||crop.width<1||crop.height<1||rotation>=3600)return t;
    const double radians=double(rotation)*3.14159265358979323846/1800.0;
    const double c=std::cos(radians),s=std::sin(radians),flip=mirror?-1.0:1.0;
    const double cw=crop.width-1,ch=crop.height-1;
    t.x[0]=float(c*flip*cw/(w-1));t.x[1]=float(s*ch/(w-1));
    t.y[0]=float(-s*flip*cw/(h-1));t.y[1]=float(c*ch/(h-1));
    t.x[2]=float((crop.left+cw*.5)/(w-1))-.5f*(t.x[0]+t.x[1]);
    t.y[2]=float((crop.top+ch*.5)/(h-1))-.5f*(t.y[0]+t.y[1]);
    return t;
}
}
