#pragma once
#include "HostWindows.h"
namespace s7 {
struct SniperTouch {
    bool down=false;
    POINT last{};
    POINTER_TOUCH_INFO frame(bool pressed,POINT position)const {
        POINTER_TOUCH_INFO f{};
        f.pointerInfo.pointerType=PT_TOUCH;f.pointerInfo.pointerId=0;
        f.pointerInfo.ptPixelLocation=pressed?position:last;
        f.pointerInfo.pointerFlags=pressed?(POINTER_FLAG_INRANGE|POINTER_FLAG_INCONTACT|(down?POINTER_FLAG_UPDATE:POINTER_FLAG_DOWN)):POINTER_FLAG_UP;
        const auto p=f.pointerInfo.ptPixelLocation;
        f.touchMask=TOUCH_MASK_CONTACTAREA|TOUCH_MASK_ORIENTATION|TOUCH_MASK_PRESSURE;
        f.rcContact={p.x-2,p.y-2,p.x+2,p.y+2};f.orientation=90;f.pressure=pressed?512:0;
        return f;
    }
    void accepted(const POINTER_TOUCH_INFO& f){last=f.pointerInfo.ptPixelLocation;down=(f.pointerInfo.pointerFlags&POINTER_FLAG_INCONTACT)!=0;}
};
}
