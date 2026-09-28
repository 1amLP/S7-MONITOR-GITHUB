#pragma once
#include "Camera.h"
#include <mfcaptureengine.h>
#include <atomic>
#include <iostream>

namespace s7camera {
struct CaptureMetadataReady final : ComObject<IMFCaptureEngineOnEventCallback> {
    HANDLE ready=CreateEventW(nullptr,TRUE,FALSE,nullptr);
    std::atomic<HRESULT> result{E_PENDING};
    ~CaptureMetadataReady(){if(ready)CloseHandle(ready);}
    STDMETHODIMP OnEvent(IMFMediaEvent* event) override {
        GUID type{};HRESULT status=S_OK;
        if(event&&SUCCEEDED(event->GetExtendedType(&type))&&type==MF_CAPTURE_ENGINE_INITIALIZED){
            const auto hr=event->GetStatus(&status);result=FAILED(hr)?hr:status;SetEvent(ready);
        }
        return S_OK;
    }
};
// Capability inspection only: never StartPreview, StartRecord or ReadSample.
inline void describeCaptureMetadata(IMFActivate* activation){
    ComPtr<IMFMediaSource> device;
    ComPtr<IMFCaptureEngine> engine;
    const auto hr=guarded([&]{
        check(activation->ActivateObject(IID_PPV_ARGS(&device)));
        ComPtr<IMFCaptureEngineClassFactory> factory;
        check(CoCreateInstance(CLSID_MFCaptureEngineClassFactory,nullptr,CLSCTX_INPROC_SERVER,IID_PPV_ARGS(&factory)));
        check(factory->CreateInstance(CLSID_MFCaptureEngine,IID_PPV_ARGS(&engine)));
        auto callback=Make<CaptureMetadataReady>();if(!callback||!callback->ready)throw E_OUTOFMEMORY;
        ComPtr<IMFAttributes> attrs;check(MFCreateAttributes(&attrs,1));
        check(attrs->SetUINT32(MF_CAPTURE_ENGINE_USE_VIDEO_DEVICE_ONLY,TRUE));
        check(engine->Initialize(callback.Get(),attrs.Get(),nullptr,device.Get()));
        if(WaitForSingleObject(callback->ready,10000)!=WAIT_OBJECT_0)throw HRESULT_FROM_WIN32(ERROR_TIMEOUT);
        check(callback->result.load());
        ComPtr<IMFCaptureSource> source;check(engine->GetSource(&source));
        DWORD count=0;check(source->GetDeviceStreamCount(&count));
        std::wcout<<L"  streams="<<count<<std::endl;
        for(DWORD i=0;i<count;++i){
            MF_CAPTURE_ENGINE_STREAM_CATEGORY category{};check(source->GetDeviceStreamCategory(i,&category));
            std::wcout<<L"  stream="<<i<<L" category="<<unsigned(category)<<std::endl;
        }
        for(auto index:{DWORD(MF_CAPTURE_ENGINE_PREFERRED_SOURCE_STREAM_FOR_VIDEO_PREVIEW),DWORD(MF_CAPTURE_ENGINE_PREFERRED_SOURCE_STREAM_FOR_VIDEO_RECORD)}){
            ComPtr<IMFMediaType> type;
            const auto result=source->GetAvailableDeviceMediaType(index,0,&type);
            std::wcout<<L"  preferred="<<std::hex<<index<<L" result=0x"<<unsigned(result)<<std::dec;
            if(SUCCEEDED(result)){
                UINT32 w=0,h=0,n=0,d=0;GUID subtype{};
                check(MFGetAttributeSize(type.Get(),MF_MT_FRAME_SIZE,&w,&h));
                check(MFGetAttributeRatio(type.Get(),MF_MT_FRAME_RATE,&n,&d));check(type->GetGUID(MF_MT_SUBTYPE,&subtype));
                wchar_t id[40]{};StringFromGUID2(subtype,id,40);
                std::wcout<<L" size="<<w<<L'x'<<h<<L" rate="<<n<<L'/'<<d<<L" subtype="<<id;
            }
            std::wcout<<std::endl;
        }
    });
    engine.Reset();
    if(device)device->Shutdown();
    activation->ShutdownObject();
    std::wcout<<L"  metadata result=0x"<<std::hex<<unsigned(hr)<<std::dec<<L"; capture not started"<<std::endl;
}
}
