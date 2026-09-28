// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once
#include "Camera.h"
#include <wincodec.h>
#include <objidl.h>
#include <vector>

namespace s7camera {
class PlanarJpegEncoder {
    ComPtr<IWICImagingFactory> factory_;
public:
    PlanarJpegEncoder(){
        HRESULT hr=CoCreateInstance(CLSID_WICImagingFactory2,nullptr,CLSCTX_INPROC_SERVER,IID_PPV_ARGS(&factory_));
        if(FAILED(hr))check(CoCreateInstance(CLSID_WICImagingFactory,nullptr,CLSCTX_INPROC_SERVER,IID_PPV_ARGS(&factory_)));
    }
    std::vector<BYTE> encode(BYTE const* nv12,size_t bytes,UINT width,UINT height) const {
        const uint64_t yBytes=uint64_t(width)*height,total=yBytes+yBytes/2;
        if(!nv12||width<2||height<2||(width|height)&1||width>4096||height>2304||total!=bytes||total>UINT_MAX)
            throw E_INVALIDARG;
        ComPtr<IStream> stream;check(CreateStreamOnHGlobal(nullptr,TRUE,&stream));
        ComPtr<IWICBitmapEncoder> encoder;check(factory_->CreateEncoder(GUID_ContainerFormatJpeg,nullptr,&encoder));
        check(encoder->Initialize(stream.Get(),WICBitmapEncoderNoCache));
        ComPtr<IWICBitmapFrameEncode> frame;ComPtr<IPropertyBag2> properties;
        check(encoder->CreateNewFrame(&frame,&properties));
        PROPBAG2 options[2]{};VARIANT values[2]{};
        options[0].pstrName=const_cast<LPOLESTR>(L"ImageQuality");values[0].vt=VT_R4;values[0].fltVal=.85f;
        options[1].pstrName=const_cast<LPOLESTR>(L"JpegYCrCbSubsampling");values[1].vt=VT_UI1;values[1].bVal=WICJpegYCrCbSubsampling420;
        check(properties->Write(2,options,values));
        check(frame->Initialize(properties.Get()));check(frame->SetSize(width,height));
        WICPixelFormatGUID format=GUID_WICPixelFormat24bppBGR;check(frame->SetPixelFormat(&format));
        if(format!=GUID_WICPixelFormat24bppBGR)throw WINCODEC_ERR_UNSUPPORTEDPIXELFORMAT;
        ComPtr<IWICBitmap> y,uv;
        check(factory_->CreateBitmapFromMemory(width,height,GUID_WICPixelFormat8bppY,width,UINT(yBytes),const_cast<BYTE*>(nv12),&y));
        check(factory_->CreateBitmapFromMemory(width/2,height/2,GUID_WICPixelFormat16bppCbCr,width,UINT(yBytes/2),const_cast<BYTE*>(nv12+yBytes),&uv));
        ComPtr<IWICPlanarBitmapFrameEncode> planar;check(frame.As(&planar));
        IWICBitmapSource* planes[]={y.Get(),uv.Get()};check(planar->WriteSource(planes,2,nullptr));
        check(frame->Commit());check(encoder->Commit());
        STATSTG stat{};check(stream->Stat(&stat,STATFLAG_NONAME));
        const ULONGLONG encoded=stat.cbSize.QuadPart;
        if(encoded<4||encoded>16ull*1024*1024||encoded>SIZE_MAX)throw WINCODEC_ERR_BADIMAGE;
        HGLOBAL memory=nullptr;check(GetHGlobalFromStream(stream.Get(),&memory));
        auto* data=static_cast<BYTE*>(GlobalLock(memory));if(!data)throw HRESULT_FROM_WIN32(GetLastError());
        struct Unlock {HGLOBAL value;~Unlock(){GlobalUnlock(value);}} unlock{memory};
        std::vector<BYTE> result(data,data+size_t(encoded));
        if(result[0]!=0xff||result[1]!=0xd8||result[result.size()-2]!=0xff||result.back()!=0xd9)throw WINCODEC_ERR_BADIMAGE;
        return result;
    }
};
}
