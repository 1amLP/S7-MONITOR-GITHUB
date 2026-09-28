#pragma once
#include "WinUsbCamera.h"
#include "SelectionProtocol.h"
#include <atomic>
#include <condition_variable>
#include <deque>
#include <thread>

namespace s7camera {
extern std::atomic<long> objects;
struct CameraSourcePin {CameraSourcePin(){++objects;}~CameraSourcePin(){--objects;}};
inline ComPtr<IMFMediaType> cameraEncodedType(WebcamMode mode){
    if(!webcamEligible(mode))throw MF_E_INVALIDMEDIATYPE;
    ComPtr<IMFMediaType> type;check(MFCreateMediaType(&type));
    check(type->SetGUID(MF_MT_MAJOR_TYPE,MFMediaType_Video));check(type->SetGUID(MF_MT_SUBTYPE,MFVideoFormat_H264));
    check(MFSetAttributeSize(type.Get(),MF_MT_FRAME_SIZE,mode.width,mode.height));check(MFSetAttributeRatio(type.Get(),MF_MT_FRAME_RATE,mode.fps,1));
    check(MFSetAttributeRatio(type.Get(),MF_MT_PIXEL_ASPECT_RATIO,1,1));check(type->SetUINT32(MF_MT_INTERLACE_MODE,MFVideoInterlace_Progressive));
    check(type->SetUINT32(MF_MT_COMPRESSED,TRUE));check(type->SetUINT32(MF_MT_FIXED_SIZE_SAMPLES,FALSE));
    check(type->SetUINT32(MF_MT_ALL_SAMPLES_INDEPENDENT,FALSE));
    check(type->SetUINT32(MF_MT_YUV_MATRIX,MFVideoTransferMatrix_BT601));check(type->SetUINT32(MF_MT_VIDEO_NOMINAL_RANGE,MFNominalRange_16_235));
    return type;
}
#define S7_CAMERA_USB_EVENTS \
STDMETHODIMP GetEvent(DWORD f,IMFMediaEvent** e) override{return events->GetEvent(f,e);} \
STDMETHODIMP BeginGetEvent(IMFAsyncCallback* c,IUnknown* s) override{return events->BeginGetEvent(c,s);} \
STDMETHODIMP EndGetEvent(IMFAsyncResult* r,IMFMediaEvent** e) override{return events->EndGetEvent(r,e);} \
STDMETHODIMP QueueEvent(MediaEventType t,REFGUID g,HRESULT h,PROPVARIANT const* p) override{return events->QueueEventParamVar(t,g,h,p);}

class UsbEncodedStream final : public ComObject<Microsoft::WRL::ChainInterfaces<IMFMediaStream,IMFMediaEventGenerator>>,public CameraSourcePin {
    std::mutex mutex;
    std::condition_variable demand;
    std::deque<ComPtr<IUnknown>> requests;
    ComPtr<IMFMediaSource> parent;
    GUID container{};uint64_t token=0;WebcamMode mode{};
    HANDLE stopEvent=CreateEventW(nullptr,TRUE,FALSE,nullptr);
    HANDLE startEvent=CreateEventW(nullptr,TRUE,FALSE,nullptr);
    bool running=false,closed=false;
    std::thread worker;
    std::shared_ptr<WinUsbCamera> transport;
    HRESULT cleanup=S_OK;
    void run(){
        const HRESULT com=CoInitializeEx(nullptr,COINIT_MULTITHREADED);
        const HRESULT result=guarded([&]{
            check(com);transport=std::make_shared<WinUsbCamera>(container,token,mode);transport->open(stopEvent);
            bool first=true;
            while(WaitForSingleObject(stopEvent,0)!=WAIT_OBJECT_0){
                ComPtr<IUnknown> request;
                {
                    std::unique_lock lock(mutex);demand.wait(lock,[&]{return !running||!requests.empty();});
                    if(!running)break;request=std::move(requests.front());requests.pop_front();
                }
                auto frame=transport->next(stopEvent);
                if(!(frame.header.mode==mode)){
                    auto type=cameraEncodedType(frame.header.mode);
                    std::lock_guard lock(mutex);
                    ComPtr<IMFMediaTypeHandler> handler;check(descriptor->GetMediaTypeHandler(&handler));
                    check(handler->SetCurrentMediaType(type.Get()));mode=frame.header.mode;
                    check(events->QueueEventParamUnk(MEStreamFormatChanged,GUID_NULL,S_OK,type.Get()));
                }
                ComPtr<IMFSample> sample;ComPtr<IMFMediaBuffer> buffer;check(MFCreateSample(&sample));
                check(MFCreateMemoryBuffer(DWORD(frame.bytes.size()),&buffer));BYTE* data=nullptr;check(buffer->Lock(&data,nullptr,nullptr));
                std::memcpy(data,frame.bytes.data(),frame.bytes.size());check(buffer->Unlock());check(buffer->SetCurrentLength(DWORD(frame.bytes.size())));
                check(sample->AddBuffer(buffer.Get()));check(sample->SetSampleTime(LONGLONG(frame.header.pts*10)));
                check(sample->SetSampleDuration(10000000/mode.fps));check(sample->SetUINT32(MFSampleExtension_CleanPoint,(frame.header.flags&wire::KeyFrame)?TRUE:FALSE));
                check(sample->SetUINT32(MFSampleExtension_Discontinuity,(first||(frame.header.flags&wire::Discontinuity))?TRUE:FALSE));first=false;
                if(request)check(sample->SetUnknown(MFSampleExtension_Token,request.Get()));
                std::lock_guard lock(mutex);
                if(running)check(events->QueueEventParamUnk(MEMediaSample,GUID_NULL,S_OK,sample.Get()));
            }
        });
        if(transport)cleanup=transport->close();
        if(FAILED(result)&&WaitForSingleObject(stopEvent,0)!=WAIT_OBJECT_0){
            std::lock_guard lock(mutex);running=false;requests.clear();
            events->QueueEventParamVar(MEError,GUID_NULL,result,nullptr);
            if(parent)parent->QueueEvent(MEError,GUID_NULL,result,nullptr);
        }
        if(SUCCEEDED(com))CoUninitialize();
    }
public:
    ComPtr<IMFMediaEventQueue> events;
    ComPtr<IMFStreamDescriptor> descriptor;
    void initialize(IMFMediaSource* source,GUID const& id,uint64_t owner,WebcamMode selected){
        if(!stopEvent||!startEvent)throw E_OUTOFMEMORY;parent=source;container=id;token=owner;mode=selected;
        check(MFCreateEventQueue(&events));auto type=cameraEncodedType(mode);
        std::vector<ComPtr<IMFMediaType>> owned;std::vector<IMFMediaType*> types;
        for(auto m:selection::modes(selection::Sensor::Rear))if(webcamEligible(m)){owned.push_back(cameraEncodedType(m));types.push_back(owned.back().Get());}
        check(MFCreateStreamDescriptor(0,DWORD(types.size()),types.data(),&descriptor));
        ComPtr<IMFMediaTypeHandler> handler;check(descriptor->GetMediaTypeHandler(&handler));check(handler->SetCurrentMediaType(type.Get()));
    }
    void begin(){
        check(stop());std::lock_guard lock(mutex);if(closed)throw MF_E_SHUTDOWN;
        cleanup=S_OK;ResetEvent(stopEvent);ResetEvent(startEvent);running=true;
        try{worker=std::thread([this]{HANDLE waits[]{stopEvent,startEvent};if(WaitForMultipleObjects(2,waits,FALSE,INFINITE)==WAIT_OBJECT_0+1)run();});}catch(...){running=false;throw;}
    }
    void releaseStart(){SetEvent(startEvent);}
    HRESULT stop(){
        {std::lock_guard lock(mutex);running=false;requests.clear();SetEvent(stopEvent);demand.notify_all();}
        if(worker.joinable())worker.join();
        if(transport){cleanup=transport->close();if(SUCCEEDED(cleanup))transport.reset();}
        return cleanup;
    }
    HRESULT close(){
        HRESULT hr=stop();if(FAILED(hr))return hr;
        std::lock_guard lock(mutex);closed=true;parent.Reset();if(events)events->Shutdown();return S_OK;
    }
    ~UsbEncodedStream(){close();if(stopEvent)CloseHandle(stopEvent);if(startEvent)CloseHandle(startEvent);}
    S7_CAMERA_USB_EVENTS
    STDMETHODIMP GetMediaSource(IMFMediaSource** out) override{std::lock_guard lock(mutex);return parent?parent.CopyTo(out):MF_E_SHUTDOWN;}
    STDMETHODIMP GetStreamDescriptor(IMFStreamDescriptor** out) override{std::lock_guard lock(mutex);return descriptor.CopyTo(out);}
    STDMETHODIMP RequestSample(IUnknown* request) override{return guarded([&]{
        std::lock_guard lock(mutex);if(closed)throw MF_E_SHUTDOWN;if(!running)throw MF_E_INVALIDREQUEST;
        if(requests.size()>=8)throw MF_E_NOTACCEPTING;requests.emplace_back(request);demand.notify_one();
    });}
};

// This source is private to SourceReader. It is not registered with FrameServer
// and never enumerates/opens a second Windows camera to obtain its bitstream.
class UsbEncodedSource final : public ComObject<Microsoft::WRL::ChainInterfaces<IMFMediaSource,IMFMediaEventGenerator>>,public CameraSourcePin {
    std::mutex mutex;
    ComPtr<UsbEncodedStream> stream;
    ComPtr<IMFPresentationDescriptor> presentation;
    bool closed=false,announced=false;
public:
    ComPtr<IMFMediaEventQueue> events;
    void initialize(GUID const& id,uint64_t owner,WebcamMode mode){
        check(MFCreateEventQueue(&events));stream=Make<UsbEncodedStream>();if(!stream)throw E_OUTOFMEMORY;
        try{
            stream->initialize(this,id,owner,mode);IMFStreamDescriptor* streams[]{stream->descriptor.Get()};
            check(MFCreatePresentationDescriptor(1,streams,&presentation));check(presentation->SelectStream(0));
        }catch(...){stream->close();stream.Reset();throw;}
    }
    ~UsbEncodedSource(){Shutdown();}
    S7_CAMERA_USB_EVENTS
    STDMETHODIMP GetCharacteristics(DWORD* flags) override{if(!flags)return E_POINTER;std::lock_guard lock(mutex);if(closed)return MF_E_SHUTDOWN;*flags=MFMEDIASOURCE_IS_LIVE;return S_OK;}
    STDMETHODIMP CreatePresentationDescriptor(IMFPresentationDescriptor** out) override{std::lock_guard lock(mutex);return closed?MF_E_SHUTDOWN:presentation->Clone(out);}
    STDMETHODIMP Start(IMFPresentationDescriptor* desc,GUID const* format,PROPVARIANT const* position) override{return guarded([&]{
        if(!desc||!position)throw E_POINTER;if(format&&*format!=GUID_NULL)throw MF_E_UNSUPPORTED_TIME_FORMAT;
        std::lock_guard lock(mutex);if(closed)throw MF_E_SHUTDOWN;
        BOOL selected=FALSE;ComPtr<IMFStreamDescriptor> requested;check(desc->GetStreamDescriptorByIndex(0,&selected,&requested));if(!selected)throw MF_E_INVALIDREQUEST;
        ComPtr<IMFMediaTypeHandler> handler,ours;ComPtr<IMFMediaType> type;
        check(requested->GetMediaTypeHandler(&handler));check(handler->GetCurrentMediaType(&type));
        check(stream->descriptor->GetMediaTypeHandler(&ours));check(ours->IsMediaTypeSupported(type.Get(),nullptr));
        check(stream->stop());
        check(events->QueueEventParamUnk(announced?MEUpdatedStream:MENewStream,GUID_NULL,S_OK,stream.Get()));
        stream->begin();PROPVARIANT zero{};zero.vt=VT_I8;zero.hVal.QuadPart=0;
        check(stream->events->QueueEventParamVar(MEStreamStarted,GUID_NULL,S_OK,&zero));
        check(events->QueueEventParamVar(MESourceStarted,GUID_NULL,S_OK,&zero));announced=true;stream->releaseStart();
    });}
    STDMETHODIMP Stop() override{return guarded([&]{
        std::lock_guard lock(mutex);if(closed)throw MF_E_SHUTDOWN;check(stream->stop());
        check(stream->events->QueueEventParamVar(MEStreamStopped,GUID_NULL,S_OK,nullptr));check(events->QueueEventParamVar(MESourceStopped,GUID_NULL,S_OK,nullptr));
    });}
    STDMETHODIMP Pause() override{return MF_E_INVALID_STATE_TRANSITION;}
    STDMETHODIMP Shutdown() override{
        std::lock_guard lock(mutex);if(closed)return S_OK;
        if(stream){const auto hr=stream->close();if(FAILED(hr))return hr;stream.Reset();}
        closed=true;if(events)events->Shutdown();return S_OK;
    }
};
#undef S7_CAMERA_USB_EVENTS
}
