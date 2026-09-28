#pragma once
#include "Camera.h"
#include "ModePolicy.h"

namespace s7camera {
struct DecodedLayout {UINT32 codedWidth,codedHeight;};

inline DecodedLayout checkedDecodedType(IMFMediaType* type,VisibleVideoFormat selected){
    if(!type)throw E_POINTER;
    GUID subtype{};check(type->GetGUID(MF_MT_SUBTYPE,&subtype));
    UINT32 codedWidth=0,codedHeight=0,rate=0,denominator=0;
    check(MFGetAttributeSize(type,MF_MT_FRAME_SIZE,&codedWidth,&codedHeight));
    check(MFGetAttributeRatio(type,MF_MT_FRAME_RATE,&rate,&denominator));
    UINT32 visibleWidth=codedWidth,visibleHeight=codedHeight;
    MFVideoArea aperture{};UINT32 apertureBytes=0;
    HRESULT apertureStatus=type->GetBlob(MF_MT_MINIMUM_DISPLAY_APERTURE,
        reinterpret_cast<UINT8*>(&aperture),sizeof(aperture),&apertureBytes);
    if(apertureStatus==MF_E_ATTRIBUTENOTFOUND)
        apertureStatus=type->GetBlob(MF_MT_GEOMETRIC_APERTURE,
            reinterpret_cast<UINT8*>(&aperture),sizeof(aperture),&apertureBytes);
    if(SUCCEEDED(apertureStatus)){
        if(apertureBytes!=sizeof(aperture)||aperture.OffsetX.value||aperture.OffsetX.fract||
           aperture.OffsetY.value||aperture.OffsetY.fract||aperture.Area.cx<=0||aperture.Area.cy<=0||
           UINT32(aperture.Area.cx)>codedWidth||UINT32(aperture.Area.cy)>codedHeight)throw MF_E_INVALIDMEDIATYPE;
        visibleWidth=UINT32(aperture.Area.cx);visibleHeight=UINT32(aperture.Area.cy);
    }else if(apertureStatus!=MF_E_ATTRIBUTENOTFOUND)check(apertureStatus);
    UINT32 pixelN=0,pixelD=0;
    const HRESULT pixelStatus=MFGetAttributeRatio(type,MF_MT_PIXEL_ASPECT_RATIO,&pixelN,&pixelD);
    if(SUCCEEDED(pixelStatus)){if(!pixelN||pixelN!=pixelD)throw MF_E_INVALIDMEDIATYPE;}
    else if(pixelStatus!=MF_E_ATTRIBUTENOTFOUND)check(pixelStatus);
    if(subtype!=MFVideoFormat_NV12||!sameVisibleVideoFormat({visibleWidth,visibleHeight,rate,denominator},selected))throw MF_E_INVALIDMEDIATYPE;
    return {codedWidth,codedHeight};
}
}
