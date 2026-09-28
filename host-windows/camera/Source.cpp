#include "Camera.h"
#include "Attributes.h"
#include "Nv12.h"
#include "DecodedType.h"
#include "Preference.h"
#include "Physical.h"
#include "Selection.h"
#include "CameraProperties.h"
#include "CaptureTimeline.h"
#include "ReaderLifetime.h"
#include "ReaderPolicy.h"
#include "CameraTrace.h"
#ifndef S7_LEGACY_FRAME_SERVER
#include "UsbEncodedSource.h"
#endif
#include "Mjpeg.h"
#include "legacy/Endpoint.h"
#include "../diagnostics/FrameRateMatch.h"
#include <algorithm>
#include <atomic>
#include <cwctype>
#include <deque>
#include <mutex>
#include <condition_variable>
#include <thread>
#include <d3d11.h>
#include <cstdio>
#include <vector>

namespace s7camera {
std::atomic<long> objects{0},locks{0};
struct Counted { Counted(){++objects;} ~Counted(){--objects;} };
struct Handle {HANDLE h;explicit Handle(bool manual=false):h(CreateEventW(nullptr,manual,FALSE,nullptr)){if(!h)throw HRESULT_FROM_WIN32(GetLastError());}~Handle(){CloseHandle(h);}Handle(Handle const&)=delete;};
constexpr DWORD firstVideo=static_cast<DWORD>(MF_SOURCE_READER_FIRST_VIDEO_STREAM);
constexpr DWORD allStreams=static_cast<DWORD>(MF_SOURCE_READER_ALL_STREAMS);

ComPtr<IMFMediaType> outputType(WebcamMode m,GUID const& subtype=MFVideoFormat_NV12){
	if(!knownMode(m))throw MF_E_INVALIDMEDIATYPE;
	if(subtype!=MFVideoFormat_NV12&&subtype!=MFVideoFormat_MJPG)throw MF_E_INVALIDMEDIATYPE;
	ComPtr<IMFMediaType> t;check(MFCreateMediaType(&t));check(t->SetGUID(MF_MT_MAJOR_TYPE,MFMediaType_Video));check(t->SetGUID(MF_MT_SUBTYPE,subtype));
	check(MFSetAttributeSize(t.Get(),MF_MT_FRAME_SIZE,m.width,m.height));check(MFSetAttributeRatio(t.Get(),MF_MT_FRAME_RATE,m.fps,1));check(MFSetAttributeRatio(t.Get(),MF_MT_PIXEL_ASPECT_RATIO,1,1));
	check(t->SetUINT32(MF_MT_INTERLACE_MODE,MFVideoInterlace_Progressive));check(t->SetUINT32(MF_MT_ALL_SAMPLES_INDEPENDENT,TRUE));
	if(subtype==MFVideoFormat_NV12){
		check(t->SetUINT32(MF_MT_DEFAULT_STRIDE,m.width));check(t->SetUINT32(MF_MT_SAMPLE_SIZE,m.width*m.height*3/2));check(t->SetUINT32(MF_MT_FIXED_SIZE_SAMPLES,TRUE));
		// Match the native FIMC/Preview contract, not the desktop's BT.709.
		check(t->SetUINT32(MF_MT_YUV_MATRIX,MFVideoTransferMatrix_BT601));
		check(t->SetUINT32(MF_MT_VIDEO_NOMINAL_RANGE,MFNominalRange_16_235));
	}else{
		check(t->SetUINT32(MF_MT_COMPRESSED,TRUE));check(t->SetUINT32(MF_MT_FIXED_SIZE_SAMPLES,FALSE));
	}
	return t;
}
ComPtr<IMFMediaType> nativeType(IMFSourceReader* reader,WebcamMode wanted){
    ComPtr<IMFMediaType> decoded;
    for(DWORD i=0;;++i){
        ComPtr<IMFMediaType> t;HRESULT hr=reader->GetNativeMediaType(firstVideo,i,&t);
        if(hr==MF_E_NO_MORE_TYPES)break;check(hr);GUID subtype{};UINT32 w=0,h=0,n=0,d=0;
        if(SUCCEEDED(t->GetGUID(MF_MT_SUBTYPE,&subtype))&&(subtype==MFVideoFormat_H264||subtype==MFVideoFormat_NV12)&&
           SUCCEEDED(MFGetAttributeSize(t.Get(),MF_MT_FRAME_SIZE,&w,&h))&&SUCCEEDED(MFGetAttributeRatio(t.Get(),MF_MT_FRAME_RATE,&n,&d))&&
           w==wanted.width&&h==wanted.height&&s7probe::exactFrameRate(n,d,wanted.fps)){
            if(subtype==MFVideoFormat_H264)return t;
            decoded=t;
        }
    }
    // An associated FrameServer source can expose already-decoded NV12.
    // Accept only the exact sensor size/cadence, never a resized substitute.
    if(decoded)return decoded;
    throw MF_E_INVALIDMEDIATYPE;
}

struct ReaderCallback final : ComObject<IMFSourceReaderCallback>,Counted {
    Handle ready,flushed{true};std::mutex mutex;HRESULT status=S_OK;DWORD flags=0;
    LONGLONG timestamp=0;ComPtr<IMFSample> frame;
    bool accepting=true,broken=false,pending=false;
    void expect(){
        std::lock_guard lock(mutex);
        if(!accepting||broken||pending)throw MF_E_INVALIDREQUEST;
        ResetEvent(ready.h);frame.Reset();pending=true;
    }
    STDMETHODIMP OnReadSample(HRESULT hr,DWORD,DWORD f,LONGLONG time,IMFSample* sample) override {
        std::lock_guard lock(mutex);
        if(f&MF_SOURCE_READERF_ERROR)broken=true;
        if(accepting){
            if(!pending){broken=true;hr=MF_E_INVALIDREQUEST;}
            pending=false;status=hr;flags=f;timestamp=time;frame=sample;SetEvent(ready.h);
        }
        return S_OK;
    }
    STDMETHODIMP OnFlush(DWORD) override{SetEvent(flushed.h);return S_OK;}
    STDMETHODIMP OnEvent(DWORD,IMFMediaEvent*) override{return S_OK;}
    bool discard(){std::lock_guard lock(mutex);accepting=false;frame.Reset();return broken;}
    ComPtr<IMFSample> take(DWORD& changes,LONGLONG& time){
        std::lock_guard lock(mutex);check(status);changes=flags;time=timestamp;
        if(broken)throw E_FAIL; // No more SourceReader methods after ERROR.
        if(flags&MF_SOURCE_READERF_ENDOFSTREAM)throw MF_E_END_OF_STREAM;
        return std::move(frame);
    }
};

// Detach the physical instance from FrameServer's activation cache. This worker
// owns its Shutdown; shutting down an activation alone is not a source barrier.
struct CameraActivation : Counted {
    ComPtr<IMFActivate> value;
    ComPtr<IMFMediaSource> active;
    bool detached=false;
    unsigned sensor=0;
    HRESULT open(ComPtr<IMFMediaSource>& source){
        if(active)return MF_E_INVALIDREQUEST;
        HRESULT hr=value->ActivateObject(IID_PPV_ARGS(&active));
        if(FAILED(hr))return hr;
        hr=value->DetachObject();
        detached=SUCCEEDED(hr);
        cameraTrace("detach physical activation",sensor,hr);
        if(FAILED(hr)&&hr!=E_NOTIMPL)return hr;
        DWORD flags=0;hr=active->GetCharacteristics(&flags);
        if(FAILED(hr)){cameraTrace("activated physical source invalid",sensor,hr);return hr;}
        return active.CopyTo(&source);
    }
    HRESULT shutdown()noexcept{
        if(!active)return S_OK;
        const HRESULT stopped=active->Shutdown();
        cameraTrace("physical source shutdown",sensor,stopped);
        if(FAILED(stopped)&&stopped!=MF_E_SHUTDOWN)return stopped;
        // Optional DetachObject may be unsupported. In that case retire the
        // cache only after the actual source has completed Shutdown.
        const HRESULT released=detached?S_OK:value->ShutdownObject();
        if(FAILED(released)&&released!=MF_E_SHUTDOWN)return released;
        active.Reset();detached=false;return S_OK;
    }
    ~CameraActivation(){const HRESULT hr=shutdown();if(FAILED(hr)&&hr!=MF_E_SHUTDOWN)cameraTrace("source activation shutdown",sensor,hr);}
};

struct ReaderResources : Counted {
    std::shared_ptr<Handle> cancel;
    selection::Sensor sensor=selection::Sensor::Rear;
    ComPtr<IMFSourceReader> reader;
    ComPtr<ReaderCallback> callback;
    ComPtr<IMFMediaSource> physical;
    std::shared_ptr<CameraActivation> activation;
    std::shared_ptr<CameraSelection> selection;
	bool poisoned=false,awaitFlush=false,readerReleased=false,physicalShutdown=false;
    HRESULT cleanup=S_OK;
    void poison(HRESULT hr)noexcept{poisoned=true;if(SUCCEEDED(cleanup))cleanup=hr;}
    bool lateFlushReady()const noexcept{
        return awaitFlush&&callback&&WaitForSingleObject(callback->flushed.h,0)==WAIT_OBJECT_0;
    }
	bool finishHardware()noexcept{
		HRESULT hr=S_OK;
		if(!physicalShutdown)hr=activation?activation->shutdown():(physical?physical->Shutdown():S_OK);
		if(FAILED(hr)&&hr!=MF_E_SHUTDOWN){poison(hr);return false;}
		physicalShutdown=true;
		physical.Reset();activation.reset();
		selection.reset();poisoned=false;cleanup=S_OK;return true;
	}
	bool recoveryReady()const noexcept{return readerReleased||lateFlushReady();}
	bool retireReady()noexcept{
		if(!recoveryReady())return false;
		if(!readerReleased){reader.Reset();callback.Reset();awaitFlush=false;readerReleased=true;}
		return finishHardware();
	}
	void finish()noexcept{
		if(!poisoned||readerReleased)finishHardware();
    }
};
inline ReaderLifetime<ReaderResources>& readerLifetime(){
    // Deliberately process-lifetime. A poisoned slot retains one reader, its
    // callback, physical source and phone lease. Counted prevents DLL unload.
    static auto* lifetime=new ReaderLifetime<ReaderResources>();return *lifetime;
}
struct ReaderOwnerGuard {
    std::shared_ptr<ReaderResources> owner;HRESULT& cleanup;
    ~ReaderOwnerGuard(){
        owner->finish();cleanup=owner->cleanup;
        readerLifetime().finish(owner,!owner->poisoned);
    }
};
// OnFlush is the normal completion barrier. If it fails, only a successful
// Shutdown of our physical source permits retiring the reader and phone lease.
class ReaderScope {
    std::shared_ptr<ReaderResources> owner;
    bool finished=false;
public:
    explicit ReaderScope(std::shared_ptr<ReaderResources> r):owner(std::move(r)){}
    HRESULT close()noexcept{
        if(finished)return owner->cleanup;finished=true;
        const bool broken=owner->callback->discard();
        HRESULT hr=S_OK;
		switch(readerCloseAction(bool(owner->reader),broken)){
		case ReaderCloseAction::ReleaseTerminal:
			// MF_SOURCE_READERF_ERROR forbids further IMFSourceReader calls.
			// Releasing COM ownership is not such a call and lets the physical
			// source and phone lease be retired without poisoning this process.
			owner->reader.Reset();owner->callback.Reset();owner->readerReleased=true;
			break;
		case ReaderCloseAction::Flush: {
			ResetEvent(owner->callback->flushed.h);
			hr=owner->reader->Flush(allStreams);
			if(SUCCEEDED(hr)){
				DWORD wait=WaitForSingleObject(owner->callback->flushed.h,2000);
				if(wait!=WAIT_OBJECT_0){owner->awaitFlush=true;hr=HRESULT_FROM_WIN32(wait==WAIT_FAILED?GetLastError():ERROR_TIMEOUT);}
			}
			break;
		}
		case ReaderCloseAction::None:break;
		}
		if(FAILED(hr)){
			// A failed Flush will never promise OnFlush. Explicitly shut down
			// only our physical source before retiring the reader and its lease.
			const HRESULT stopped=owner->activation?owner->activation->shutdown():(owner->physical?owner->physical->Shutdown():S_OK);
			cameraTrace("reader flush failed; owned source shutdown",0,hr,stopped);
			if(FAILED(stopped)&&stopped!=MF_E_SHUTDOWN){owner->poison(stopped);return stopped;}
			owner->physicalShutdown=true;owner->awaitFlush=false;
		}
		if(!owner->readerReleased){owner->reader.Reset();owner->callback.Reset();owner->readerReleased=true;}
		return S_OK;
    }
    ~ReaderScope(){close();}
};

struct NV12Geometry {UINT32 width=0,height=0;LONG stride=0;};
static NV12Geometry decodedGeometry(IMFSourceReader* reader,IMFMediaBuffer* buffer,
                                   UINT32 width,UINT32 height,UINT32 rate,UINT32 denominator){
    ComPtr<IMFMediaType> actual;check(reader->GetCurrentMediaType(firstVideo,&actual));
    const auto decoded=checkedDecodedType(actual.Get(),{width,height,rate,denominator});
    NV12Geometry g{decoded.codedWidth,decoded.codedHeight};
    UINT32 rawStride=g.width;
    HRESULT hr=actual->GetUINT32(MF_MT_DEFAULT_STRIDE,&rawStride);
    if(FAILED(hr)&&hr!=MF_E_ATTRIBUTENOTFOUND)check(hr);
    g.stride=static_cast<LONG>(rawStride);
    ComPtr<IMFDXGIBuffer> dxgi;
    if(SUCCEEDED(buffer->QueryInterface(IID_PPV_ARGS(&dxgi)))){
        ComPtr<ID3D11Texture2D> texture;check(dxgi->GetResource(IID_PPV_ARGS(&texture)));
        D3D11_TEXTURE2D_DESC desc{};texture->GetDesc(&desc);
        UINT subresource=0;check(dxgi->GetSubresourceIndex(&subresource));
        if(desc.Format!=DXGI_FORMAT_NV12||desc.MipLevels!=1||desc.SampleDesc.Count!=1||subresource>=desc.ArraySize||
           desc.Width<g.width||desc.Height<g.height)throw MF_E_INVALIDMEDIATYPE;
        // Texture allocation height, not visible 1080, determines where UV starts.
        g.width=desc.Width;g.height=desc.Height;
    }
    if(g.width<width||g.height<height||g.width>4096||g.height>2304||((g.width|g.height)&1))throw MF_E_INVALIDMEDIATYPE;
    return g;
}
static void copyDecodedNV12(IMFSourceReader* reader,IMFMediaBuffer* from,BYTE* destination,
                            DWORD bytes,UINT32 width,UINT32 height,UINT32 rate,UINT32 denominator,
                            std::vector<BYTE>& contiguous){
    const auto geometry=decodedGeometry(reader,from,width,height,rate,denominator);
    ComPtr<IMF2DBuffer2> twoD2;
    if(SUCCEEDED(from->QueryInterface(IID_PPV_ARGS(&twoD2)))){
        BYTE *scan=nullptr,*base=nullptr;LONG pitch=0;DWORD length=0;
        check(twoD2->Lock2DSize(MF2DBuffer_LockFlags_Read,&scan,&pitch,&base,&length));
        const HRESULT copied=guarded([&]{
            const auto start=reinterpret_cast<uintptr_t>(base),top=reinterpret_cast<uintptr_t>(scan);
            if(pitch<=0||top<start||top-start>length||
               !copyNV12Rows(base,length,size_t(top-start),size_t(pitch),geometry.height,
                             destination,bytes,width,height))throw MF_E_INVALIDMEDIATYPE;
        });
        const HRESULT unlocked=twoD2->Unlock2D();check(copied);check(unlocked);return;
    }
    ComPtr<IMF2DBuffer> twoD;
    if(SUCCEEDED(from->QueryInterface(IID_PPV_ARGS(&twoD)))){
        DWORD length=0;check(twoD->GetContiguousLength(&length));
        if(length!=size_t(geometry.width)*geometry.height*3/2)throw MF_E_INVALIDMEDIATYPE;
        if(geometry.width==width&&geometry.height==height){check(twoD->ContiguousCopyTo(destination,bytes));return;}
        contiguous.resize(length);check(twoD->ContiguousCopyTo(contiguous.data(),length));
        if(!visibleNV12(contiguous.data(),length,geometry.width,geometry.height,destination,bytes,width,height))throw MF_E_INVALIDMEDIATYPE;
        return;
    }
    BYTE* data=nullptr;DWORD length=0;check(from->Lock(&data,nullptr,&length));
    const bool copied=geometry.stride>0&&copyNV12Rows(data,length,0,size_t(geometry.stride),geometry.height,
                                                       destination,bytes,width,height);
    const HRESULT unlocked=from->Unlock();if(!copied)throw MF_E_INVALIDMEDIATYPE;check(unlocked);
}

#define S7_EVENTS \
STDMETHODIMP GetEvent(DWORD f,IMFMediaEvent** e) override{return events->GetEvent(f,e);} \
STDMETHODIMP BeginGetEvent(IMFAsyncCallback* c,IUnknown* s) override{return events->BeginGetEvent(c,s);} \
STDMETHODIMP EndGetEvent(IMFAsyncResult* r,IMFMediaEvent** e) override{return events->EndGetEvent(r,e);} \
STDMETHODIMP QueueEvent(MediaEventType t,REFGUID g,HRESULT h,PROPVARIANT const* p) override{return events->QueueEventParamVar(t,g,h,p);}

class Stream final : public ComObject<Microsoft::WRL::ChainInterfaces<IMFMediaStream2,IMFMediaStream,IMFMediaEventGenerator>>,public Counted {
    std::recursive_mutex lifecycle;
    std::condition_variable demand;
    std::mutex mutex;std::deque<ComPtr<IUnknown>> requests;Handle stopEvent{true},startEvent{true};std::thread worker;
    std::shared_ptr<Handle> captureCancel;
	ComPtr<IMFMediaSource> parent;std::shared_ptr<CameraActivation> physicalActivation;ComPtr<IMFMediaType> mediaType;ComPtr<IUnknown> manager;
	selection::Sensor sensor=selection::Sensor::Rear;
	bool running=false,paused=false,capturing=false,shutdown=false;HRESULT lastError=S_OK;UINT32 width=0,height=0,rate=0,denominator=0;GUID subtype=MFVideoFormat_NV12;
    std::vector<WebcamMode> advertised;
    void changeFormat(IMFMediaType* type,WebcamMode mode){
        std::lock_guard lock(mutex);
        if(!running)return;
        if(std::find(advertised.begin(),advertised.end(),mode)==advertised.end())advertised.push_back(mode);
        std::vector<ComPtr<IMFMediaType>> owned;std::vector<IMFMediaType*> types;
        for(auto m:advertised){owned.push_back(outputType(m,MFVideoFormat_NV12));types.push_back(owned.back().Get());owned.push_back(outputType(m,MFVideoFormat_MJPG));types.push_back(owned.back().Get());}
        ComPtr<IMFStreamDescriptor> next;check(MFCreateStreamDescriptor(0,DWORD(types.size()),types.data(),&next));
        check(attributes->CopyAllItems(next.Get()));ComPtr<IMFMediaTypeHandler> handler;check(next->GetMediaTypeHandler(&handler));check(handler->SetCurrentMediaType(type));
        width=mode.width;height=mode.height;rate=mode.fps;denominator=1;mediaType=type;descriptor=next;
        check(events->QueueEventParamUnk(MEStreamFormatChanged,GUID_NULL,S_OK,type));
    }
    bool decode(){
        HRESULT init=CoInitializeEx(nullptr,COINIT_MULTITHREADED);
        char const* stage="reader initialization";
        HRESULT cleanup=S_OK;
        bool idle=false;
        CaptureTimeline timeline(30,1);
        HRESULT result=guarded([&]{
            check(init);
            if(!denominator||rate%denominator)throw MF_E_INVALIDMEDIATYPE;
            timeline=CaptureTimeline(rate/denominator,1);
            // Recovery executes on this COM-initialized capture worker, never in
            // DllCanUnloadNow or on a UI callback. No second reader overlaps it.
			readerLifetime().recover([](ReaderResources const& old){return old.recoveryReady();},
			                         [](ReaderResources& old){return old.retireReady();});
            stage="acquire process reader owner";
            auto owner=std::make_shared<ReaderResources>();
            owner->cancel=captureCancel;owner->sensor=sensor;
            const auto ownerDeadline=GetTickCount64()+3000;
            while(!readerLifetime().acquire(owner)){
                readerLifetime().requestHandoff([&](ReaderResources& old){
                    if(!old.cancel||old.cancel==captureCancel)return false;
                    SetEvent(old.cancel->h);return true;
                });
                if(GetTickCount64()>=ownerDeadline)throw HRESULT_FROM_WIN32(ERROR_BUSY);
                const DWORD wait=WaitForSingleObject(stopEvent.h,20);
                if(wait==WAIT_OBJECT_0)throw HRESULT_FROM_WIN32(ERROR_CANCELLED);
                if(wait==WAIT_FAILED)throw HRESULT_FROM_WIN32(GetLastError());
                readerLifetime().recover([](ReaderResources const& old){return old.recoveryReady();},
                                         [](ReaderResources& old){return old.retireReady();});
            }
            ReaderOwnerGuard ownerGuard{owner,cleanup};
            owner->selection=std::make_shared<CameraSelection>(sensor,WebcamMode{width,height,rate/denominator});
            auto& selectedCamera=*owner->selection;
            stage="claim selected S7 sensor and native mode";
            selectedCamera.acquire(captureCancel->h);
            {std::lock_guard lock(mutex);capturing=true;}
            stage="activate owned physical source";
#ifndef S7_LEGACY_FRAME_SERVER
            {
                auto source=Make<UsbEncodedSource>();if(!source)throw E_OUTOFMEMORY;
                source->initialize(selectedCamera.containerId(),selectedCamera.leaseToken(),WebcamMode{width,height,rate/denominator});
                owner->physical=source;
            }
#else
            if(physicalActivation){
                owner->activation=physicalActivation;
                check(owner->activation->open(owner->physical));
            }else{
                owner->physical=physicalCamera();
            }
#endif
            WebcamMode preferred{width,height,rate/denominator};
            if(!nativeOutput(preferred,width,height,rate,denominator))throw MF_E_INVALIDMEDIATYPE;
            auto callback=Make<ReaderCallback>();if(!callback)throw E_OUTOFMEMORY;
            owner->callback=callback;ReaderScope readerScope(owner);
            ComPtr<IMFAttributes> options;check(MFCreateAttributes(&options,6));
            check(options->SetUnknown(MF_SOURCE_READER_ASYNC_CALLBACK,callback.Get()));
            check(options->SetUINT32(MF_READWRITE_ENABLE_HARDWARE_TRANSFORMS,TRUE));
            check(options->SetUINT32(MF_LOW_LATENCY,TRUE));
            check(options->SetUINT32(MF_SOURCE_READER_ENABLE_ADVANCED_VIDEO_PROCESSING,TRUE));
            check(options->SetUINT32(MF_SOURCE_READER_DISCONNECT_MEDIASOURCE_ON_SHUTDOWN,TRUE));
            if(manager)check(options->SetUnknown(MF_SOURCE_READER_D3D_MANAGER,manager.Get()));
            auto& reader=owner->reader;
            stage="create source reader";
            check(MFCreateSourceReaderFromMediaSource(owner->physical.Get(),options.Get(),&reader));
            stage="select physical stream";
            check(reader->SetStreamSelection(allStreams,FALSE));check(reader->SetStreamSelection(firstVideo,TRUE));
            stage="find exact physical mode";
            auto native=nativeType(reader.Get(),preferred);ComPtr<IMFSourceReaderEx> extended;check(reader.As(&extended));DWORD nativeFlags=0;
            stage="native UVC mode";check(extended->SetNativeMediaType(firstVideo,native.Get(),&nativeFlags));
            // Keep the exact fraction returned by USB (e.g. 10000000/333333).
            // Requesting a rounded integer here could insert a rate converter.
            UINT32 captureRate=0,captureDenominator=0;
            check(MFGetAttributeRatio(native.Get(),MF_MT_FRAME_RATE,&captureRate,&captureDenominator));
			auto readerType=outputType(preferred,MFVideoFormat_NV12);
			check(MFSetAttributeRatio(readerType.Get(),MF_MT_FRAME_RATE,captureRate,captureDenominator));
			stage="NV12 output type";check(reader->SetCurrentMediaType(firstVideo,nullptr,readerType.Get()));
			const bool outputMJPEG=subtype==MFVideoFormat_MJPG;
			std::unique_ptr<PlanarJpegEncoder> jpegEncoder;
			if(outputMJPEG)jpegEncoder=std::make_unique<PlanarJpegEncoder>();
			std::vector<BYTE> contiguous,nv12;auto lastPreference=GetTickCount64();
            auto reportAt=lastPreference;auto reported=timeline.stats();
            ULONGLONG noRequestSince=0;
            while(WaitForSingleObject(captureCancel->h,0)!=WAIT_OBJECT_0){
                stage="read frame";callback->expect();
                check(reader->ReadSample(firstVideo,0,nullptr,nullptr,nullptr,nullptr));
                HANDLE waits[]={captureCancel->h,callback->ready.h};DWORD signaled=WAIT_TIMEOUT;auto deadline=GetTickCount64()+5000;
                do{
                    signaled=WaitForMultipleObjects(2,waits,FALSE,100);
                    if(GetTickCount64()-lastPreference>=500){lastPreference=GetTickCount64();selectedCamera.heartbeat(captureCancel->h);}
                }while(signaled==WAIT_TIMEOUT&&GetTickCount64()<deadline);
                if(signaled==WAIT_OBJECT_0)break;
                if(signaled!=WAIT_OBJECT_0+1)throw HRESULT_FROM_WIN32(signaled==WAIT_FAILED?GetLastError():ERROR_TIMEOUT);
                DWORD changes=0;LONGLONG sourceTime=0;auto input=callback->take(changes,sourceTime);
#ifndef S7_LEGACY_FRAME_SERVER
                if(changes&MF_SOURCE_READERF_NATIVEMEDIATYPECHANGED){
                    stage="negotiate phone format change";
                    ComPtr<IMFMediaType> changed;check(reader->GetNativeMediaType(firstVideo,static_cast<DWORD>(MF_SOURCE_READER_CURRENT_TYPE_INDEX),&changed));
                    UINT32 w=0,h=0,n=0,d=0;GUID compressed{};
                    check(changed->GetGUID(MF_MT_SUBTYPE,&compressed));check(MFGetAttributeSize(changed.Get(),MF_MT_FRAME_SIZE,&w,&h));
                    check(MFGetAttributeRatio(changed.Get(),MF_MT_FRAME_RATE,&n,&d));
                    if(compressed!=MFVideoFormat_H264||!d||n%d||!webcamEligible({w,h,n/d}))throw MF_E_INVALIDMEDIATYPE;
                    const WebcamMode next{w,h,n/d};
                    auto decoded=outputType(next,MFVideoFormat_NV12);
                    check(reader->SetCurrentMediaType(firstVideo,nullptr,decoded.Get()));
                    auto output=outputType(next,subtype);
                    changeFormat(output.Get(),next);
                    captureRate=n;captureDenominator=d;contiguous.clear();nv12.clear();timeline.setRate(n/d,1);
                    // The callback may include a sample in the former decoder output type.
                    // Renegotiate before requesting the next sample; never relabel its bytes.
                    continue;
                }
#else
                if(changes&MF_SOURCE_READERF_NATIVEMEDIATYPECHANGED)throw MF_E_INVALIDMEDIATYPE;
#endif
                if(changes&MF_SOURCE_READERF_STREAMTICK)timeline.gap();
                if(changes&MF_SOURCE_READERF_CURRENTMEDIATYPECHANGED){contiguous.clear();timeline.gap();}
                if(!input)continue;
                if(timeline.stats().observed==0)cameraTrace("first physical frame",unsigned(sensor),S_OK);
                stage="validate source timestamps";
                LONGLONG sampleTime=0;check(input->GetSampleTime(&sampleTime));
                if(sampleTime!=sourceTime)throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);
                UINT32 gap=FALSE;HRESULT gapResult=input->GetUINT32(MFSampleExtension_Discontinuity,&gap);
                if(FAILED(gapResult)&&gapResult!=MF_E_ATTRIBUTENOTFOUND)check(gapResult);
                // Observe even frames discarded for lack of a client request. Source
                // and arrival counters stay separate; identical source PTS is an error.
                timeline.observe(sourceTime,MFGetSystemTime(),gap!=FALSE);
                const auto reportNow=GetTickCount64();
                if(reportNow-reportAt>=5000){
                    const auto counts=timeline.stats();char detail[256]{};
                    std::snprintf(detail,sizeof(detail),"frames %ux%u@%u window_ms=%llu decoded=%llu delivered=%llu no_request=%llu gaps=%llu",
                        width,height,rate/denominator,static_cast<unsigned long long>(reportNow-reportAt),
                        static_cast<unsigned long long>(counts.observed-reported.observed),
                        static_cast<unsigned long long>(counts.delivered-reported.delivered),
                        static_cast<unsigned long long>(counts.withoutRequest-reported.withoutRequest),
                        static_cast<unsigned long long>(counts.gaps-reported.gaps));
                    cameraTrace(detail,unsigned(sensor),S_OK);reportAt=reportNow;reported=counts;
                }
                ComPtr<IUnknown> token;
                {std::lock_guard lock(mutex);
                    if(!running)break;
                    if(requests.empty()){
                        timeline.noRequest();
                        const auto now=GetTickCount64();
                        if(!noRequestSince)noRequestSince=now;
                        if(now-noRequestSince>=500){idle=true;break;}
                        continue;
                    }
                    noRequestSince=0;
                    token=std::move(requests.front());requests.pop_front();
                }
				ComPtr<IMFSample> output;check(MFCreateSample(&output));
				ComPtr<IMFMediaBuffer> from,to;DWORD bufferCount=0;check(input->GetBufferCount(&bufferCount));
				if(bufferCount==1)check(input->GetBufferByIndex(0,&from));
				else check(input->ConvertToContiguousBuffer(&from));
				const DWORD bytes=width*height*3/2;
				if(outputMJPEG){
					nv12.resize(bytes);stage="copy decoded NV12 for MJPEG";
					copyDecodedNV12(reader.Get(),from.Get(),nv12.data(),bytes,width,height,captureRate,captureDenominator,contiguous);
					stage="encode planar MJPEG on PC";const auto jpeg=jpegEncoder->encode(nv12.data(),nv12.size(),width,height);
					check(MFCreateMemoryBuffer(DWORD(jpeg.size()),&to));BYTE* destination=nullptr;check(to->Lock(&destination,nullptr,nullptr));
					std::memcpy(destination,jpeg.data(),jpeg.size());const HRESULT unlocked=to->Unlock();check(unlocked);check(to->SetCurrentLength(DWORD(jpeg.size())));
				}else{
					check(MFCreateMemoryBuffer(bytes,&to));BYTE* destination=nullptr;check(to->Lock(&destination,nullptr,nullptr));
					stage="copy bounded decoded NV12";
					HRESULT copied=guarded([&]{copyDecodedNV12(reader.Get(),from.Get(),destination,bytes,width,height,captureRate,captureDenominator,contiguous);});
					const HRESULT unlocked=to->Unlock();check(copied);check(unlocked);check(to->SetCurrentLength(bytes));
				}
				check(output->AddBuffer(to.Get()));
                const auto timing=timeline.pending();
                check(output->SetSampleTime(timing.time));check(output->SetSampleDuration(timing.duration));
                check(output->SetUINT32(MFSampleExtension_Discontinuity,timing.discontinuity?TRUE:FALSE));
                if(token)check(output->SetUnknown(MFSampleExtension_Token,token.Get()));
                check(output->SetUINT32(MFSampleExtension_CleanPoint,TRUE));
                {std::lock_guard lock(mutex);
                    if(running){check(events->QueueEventParamUnk(MEMediaSample,GUID_NULL,S_OK,output.Get()));timeline.deliver();}
                    else timeline.noRequest();
                }
            }
            stage="finish pending frames";check(readerScope.close());
        });
		if(FAILED(cleanup)){
			OutputDebugStringA("S7Camera reader ownership retained until late OnFlush or terminal-reader cleanup succeeds; no shared service restart performed\n");
            if(SUCCEEDED(result)){result=cleanup;stage="reader completion barrier";}
        }
        const auto counts=timeline.stats();char timingLog[512]{};
        if(FAILED(result)||FAILED(cleanup))cameraTrace(stage,unsigned(sensor),result,cleanup);
        std::snprintf(timingLog,sizeof(timingLog),"S7Camera timing: source=%llu delivered=%llu no_request=%llu invalid=%llu gaps=%llu source_first=%lld source_last=%lld arrival_first=%lld arrival_last=%lld units=100ns; not sensor exposure proof\n",
            static_cast<unsigned long long>(counts.observed),static_cast<unsigned long long>(counts.delivered),
            static_cast<unsigned long long>(counts.withoutRequest),static_cast<unsigned long long>(counts.invalid),
            static_cast<unsigned long long>(counts.gaps),static_cast<long long>(counts.firstSource),static_cast<long long>(counts.lastSource),
            static_cast<long long>(counts.firstArrival),static_cast<long long>(counts.lastArrival));OutputDebugStringA(timingLog);
        if(FAILED(result)&&WaitForSingleObject(stopEvent.h,0)!=WAIT_OBJECT_0&&WaitForSingleObject(captureCancel->h,0)!=WAIT_OBJECT_0){
            char text[256]{};std::snprintf(text,sizeof(text),"S7Camera %s HRESULT=%08lx\n",stage,static_cast<unsigned long>(result));
            std::fputs(text,stderr);OutputDebugStringA(text);
            {std::lock_guard lock(mutex);running=false;lastError=result;requests.clear();events->QueueEventParamVar(MEError,GUID_NULL,result,nullptr);}
            if(parent)parent->QueueEvent(MEError,GUID_NULL,result,nullptr);
        }
        if(SUCCEEDED(init))CoUninitialize();
        {std::lock_guard lock(mutex);capturing=false;}
        if(idle&&SUCCEEDED(result)&&SUCCEEDED(cleanup)){
            cameraTrace("idle capture released",unsigned(sensor),S_OK);
            return true;
        }
        return false;
    }

public:
    ComPtr<IMFMediaEventQueue> events;ComPtr<IMFStreamDescriptor> descriptor;ComPtr<IMFAttributes> attributes;
    void initialize(IMFMediaSource* owner,std::shared_ptr<CameraActivation> const& device,IMFMediaType* type,selection::Sensor selected,std::vector<WebcamMode> const& available){
 sensor=selected;
        advertised=available;
        parent=owner;physicalActivation=device;mediaType=type;
        check(MFGetAttributeSize(type,MF_MT_FRAME_SIZE,&width,&height));check(MFGetAttributeRatio(type,MF_MT_FRAME_RATE,&rate,&denominator));
        if(!width||!height||width>2560||height>1440||!rate||!denominator)throw MF_E_INVALIDMEDIATYPE;
        check(MFCreateEventQueue(&events));check(MFCreateAttributes(&attributes,4));
        check(attributes->SetGUID(MF_DEVICESTREAM_STREAM_CATEGORY,PINNAME_VIDEO_CAPTURE));
        check(attributes->SetUINT32(MF_DEVICESTREAM_STREAM_ID,0));
        check(attributes->SetUINT32(MF_DEVICESTREAM_FRAMESERVER_SHARED,1));
        check(attributes->SetUINT32(MF_DEVICESTREAM_ATTRIBUTE_FRAMESOURCE_TYPES,MFFrameSourceTypes_Color));
        std::vector<ComPtr<IMFMediaType>> owned;std::vector<IMFMediaType*> types;
        // The caller supplies provider-admitted or legacy-declared normal modes.
        // Every actual capture still requires a fresh phone admission and the
        // identical native H264 dimensions/rate (including exact UVC interval).
		for(auto m:available){
			owned.push_back(outputType(m,MFVideoFormat_NV12));types.push_back(owned.back().Get());
			owned.push_back(outputType(m,MFVideoFormat_MJPG));types.push_back(owned.back().Get());
		}
        if(types.empty())throw MF_E_INVALIDMEDIATYPE;
        check(MFCreateStreamDescriptor(0,DWORD(types.size()),types.data(),&descriptor));
        check(attributes->CopyAllItems(descriptor.Get()));
        ComPtr<IMFMediaTypeHandler> handler;check(descriptor->GetMediaTypeHandler(&handler));check(handler->SetCurrentMediaType(mediaType.Get()));
    }
    ~Stream(){stop();}
    bool failed(){std::lock_guard lock(mutex);return FAILED(lastError);}
    bool captures(){std::lock_guard lock(mutex);return running&&capturing;}
    void select(IMFMediaType* selected){
        std::lock_guard control(lifecycle);
        if(!selected)throw E_POINTER;
        GUID major{},sub{};UINT32 w=0,h=0,n=0,d=0;
        check(selected->GetGUID(MF_MT_MAJOR_TYPE,&major));check(selected->GetGUID(MF_MT_SUBTYPE,&sub));
        check(MFGetAttributeSize(selected,MF_MT_FRAME_SIZE,&w,&h));check(MFGetAttributeRatio(selected,MF_MT_FRAME_RATE,&n,&d));
		if(major!=MFMediaType_Video||(sub!=MFVideoFormat_NV12&&sub!=MFVideoFormat_MJPG)||!d||n%d||!knownMode({w,h,n/d}))throw MF_E_INVALIDMEDIATYPE;
		stop();std::lock_guard lock(mutex);mediaType=selected;width=w;height=h;rate=n;denominator=d;subtype=sub;
    }
    void stop(){
        std::lock_guard control(lifecycle);
        {std::lock_guard lock(mutex);running=false;paused=false;requests.clear();SetEvent(stopEvent.h);if(captureCancel)SetEvent(captureCancel->h);demand.notify_all();}
        if(worker.joinable())worker.join();
    }
    void close(){std::lock_guard control(lifecycle);stop();std::lock_guard lock(mutex);shutdown=true;if(events)events->Shutdown();parent.Reset();physicalActivation.reset();}
    void begin(){
        std::lock_guard control(lifecycle);
        stop();std::lock_guard lock(mutex);if(shutdown)throw MF_E_SHUTDOWN;
        captureCancel=std::make_shared<Handle>(true);
        ResetEvent(stopEvent.h);ResetEvent(startEvent.h);lastError=S_OK;running=true;
        try{worker=std::thread([this]{HANDLE waits[]={stopEvent.h,startEvent.h};
            if(WaitForMultipleObjects(2,waits,FALSE,INFINITE)!=WAIT_OBJECT_0+1)return;
            for(;;){
                {std::unique_lock lock(mutex);demand.wait(lock,[&]{return !running||!requests.empty();});if(!running)return;}
                if(!decode())return;
            }
        });}
        catch(...){running=false;SetEvent(stopEvent.h);throw;}
    }
    void releaseStart(){SetEvent(startEvent.h);}
    void setManager(IUnknown* supplied){std::lock_guard lock(mutex);if(running)throw MF_E_INVALIDREQUEST;manager=supplied;}
    S7_EVENTS
    STDMETHODIMP GetMediaSource(IMFMediaSource** source) override{std::lock_guard lock(mutex);return parent?parent.CopyTo(source):MF_E_SHUTDOWN;}
    STDMETHODIMP GetStreamDescriptor(IMFStreamDescriptor** output) override{std::lock_guard lock(mutex);return descriptor.CopyTo(output);}
    STDMETHODIMP RequestSample(IUnknown* token) override{return guarded([&]{std::lock_guard lock(mutex);if(shutdown)throw MF_E_SHUTDOWN;if(!running)throw MF_E_INVALIDREQUEST;if(requests.size()>=8)throw MF_E_NOTACCEPTING;requests.emplace_back(token);demand.notify_one();});}
    STDMETHODIMP SetStreamState(MF_STREAM_STATE state) override{return guarded([&]{
        std::lock_guard control(lifecycle);
        {std::lock_guard lock(mutex);if(shutdown)throw MF_E_SHUTDOWN;
            const auto current=running?MF_STREAM_STATE_RUNNING:(paused?MF_STREAM_STATE_PAUSED:MF_STREAM_STATE_STOPPED);
            if(state==current)return;
            if(state==MF_STREAM_STATE_PAUSED&&!running)throw MF_E_INVALID_STATE_TRANSITION;
        }
        switch(state){
        case MF_STREAM_STATE_RUNNING:begin();releaseStart();break;
        case MF_STREAM_STATE_STOPPED:stop();break;
        case MF_STREAM_STATE_PAUSED:stop();{std::lock_guard lock(mutex);paused=true;}break;
        default:throw MF_E_INVALID_STATE_TRANSITION;
        }
        cameraTrace(state==MF_STREAM_STATE_RUNNING?"stream state RUNNING":"stream state not RUNNING",unsigned(sensor),S_OK);
    });}
    STDMETHODIMP GetStreamState(MF_STREAM_STATE* state) override{if(!state)return E_POINTER;std::lock_guard lock(mutex);if(shutdown)return MF_E_SHUTDOWN;*state=running?MF_STREAM_STATE_RUNNING:(paused?MF_STREAM_STATE_PAUSED:MF_STREAM_STATE_STOPPED);return S_OK;}
};

class Source final : public ComObject<Microsoft::WRL::ChainInterfaces<IMFMediaSourceEx,IMFMediaSource,IMFMediaEventGenerator>,IMFGetService,IKsControl,IMFSampleAllocatorControl>,public Counted {
	CameraProperties properties;
    std::recursive_mutex mutex;ComPtr<Stream> stream;ComPtr<IMFPresentationDescriptor> presentation;ComPtr<IMFAttributes> attributes;ComPtr<IUnknown> deviceManager;std::shared_ptr<CameraActivation> cameraActivation;
    selection::Sensor sensor=selection::Sensor::Rear;
 bool shutdown=false,started=false,running=false;void valid(){if(shutdown)throw MF_E_SHUTDOWN;}
    void ready(){valid();if(!stream)throw MF_E_NOT_INITIALIZED;}
    void refresh(){
        if(stream){stream->close();stream.Reset();}
        presentation.Reset();started=false;running=false;
        try{
#ifdef S7_LEGACY_FRAME_SERVER
            // Declarative normal modes, not evidence that a disconnected phone
            // supports a mode. The phone's fresh claim is mandatory in decode().
            auto available=legacy::declaredModes(sensor);
#else
            auto capabilities=CameraControl().status();
            auto available=selection::admitted(capabilities,sensor);
#endif
            if(available.empty())throw MF_E_INVALIDMEDIATYPE;
            ComPtr<IMFSensorProfileCollection> profiles;
            check(MFCreateSensorProfileCollection(&profiles));
            auto addProfile=[&](GUID const& id,const wchar_t* filter,uint32_t minimumRate=0){
                ComPtr<IMFSensorProfile> profile;
                check(MFCreateSensorProfile(id,0,nullptr,&profile));
                check(profile->AddProfileFilter(0,filter));
                for(auto m:available)for(auto const& subtype:{MFVideoFormat_NV12,MFVideoFormat_MJPG}){
                    auto type=outputType(m,subtype);BOOL supported=FALSE;
                    check(profile->IsMediaTypeSupported(0,type.Get(),&supported));
                    if(bool(supported)!=(m.fps>=minimumRate))throw MF_E_INVALIDMEDIATYPE;
                }
                check(profiles->AddProfile(profile.Get()));
            };
            // Filters select existing stream types, never manufacture formats.
            // Legacy keeps all admitted modes available to non-profile clients.
            addProfile(KSCAMERAPROFILE_Legacy,L"((RES==;FRT==;SUT==))");
            addProfile(KSCAMERAPROFILE_VideoRecording,L"((RES==;FRT==;SUT==))");
            if(std::any_of(available.begin(),available.end(),[](auto m){return m.fps>60;}))
                addProfile(KSCAMERAPROFILE_HighFrameRate,L"((RES==;FRT>=120,1;SUT==))",120);
            check(attributes->SetUnknown(MF_DEVICEMFT_SENSORPROFILE_COLLECTION,profiles.Get()));
            auto decoded=outputType(available.front());
            stream=Make<Stream>();if(!stream)throw E_OUTOFMEMORY;stream->initialize(this,cameraActivation,decoded.Get(),sensor,available);stream->setManager(deviceManager.Get());
            IMFStreamDescriptor* streams[]={stream->descriptor.Get()};check(MFCreatePresentationDescriptor(1,streams,&presentation));check(presentation->SelectStream(0));
        }catch(...){if(stream){stream->close();stream.Reset();}throw;}
    }
public:
    ComPtr<IMFMediaEventQueue> events;
    void initialize(IMFAttributes* activation,selection::Sensor selected){
 sensor=selected;
        check(MFCreateEventQueue(&events));check(MFCreateAttributes(&attributes,8));if(activation)check(activation->CopyAllItems(attributes.Get()));
        // FrameServer's activation-scoped proxy must not escape again through
        // media-source attributes and become a cached association on the next open.
        check(attributes->DeleteItem(MF_VIRTUALCAMERA_ASSOCIATED_CAMERA_SOURCES));
        check(attributes->DeleteItem(MF_VIRTUALCAMERA_PROVIDE_ASSOCIATED_CAMERA_SOURCES));
        check(attributes->SetGUID(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE,MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID));
        check(attributes->SetString(MF_DEVSOURCE_ATTRIBUTE_FRIENDLY_NAME,endpointName(sensor)));
        refresh();
    }
    ~Source(){Shutdown();}
    bool closed(){std::lock_guard lock(mutex);return shutdown;}
    S7_EVENTS
    STDMETHODIMP GetCharacteristics(DWORD* flags) override{return guarded([&]{if(!flags)throw E_POINTER;std::lock_guard lock(mutex);valid();*flags=MFMEDIASOURCE_IS_LIVE;});}
    STDMETHODIMP CreatePresentationDescriptor(IMFPresentationDescriptor** output) override{return guarded([&]{std::lock_guard lock(mutex);ready();if(!output)throw E_POINTER;
        ComPtr<IMFStreamDescriptor> current;check(stream->GetStreamDescriptor(&current));IMFStreamDescriptor* streams[]{current.Get()};
        ComPtr<IMFPresentationDescriptor> fresh;check(MFCreatePresentationDescriptor(1,streams,&fresh));check(fresh->SelectStream(0));check(fresh.CopyTo(output));});}
    STDMETHODIMP Start(IMFPresentationDescriptor* requested,GUID const* format,PROPVARIANT const* position) override{return guarded([&]{
        if(!requested||!position)throw E_POINTER;if(format&&*format!=GUID_NULL)throw MF_E_UNSUPPORTED_TIME_FORMAT;
        std::lock_guard lock(mutex);ready();BOOL selected=FALSE;ComPtr<IMFStreamDescriptor> sd;check(requested->GetStreamDescriptorByIndex(0,&selected,&sd));
        if(!selected){
            stream->stop();running=false;
            PROPVARIANT zero{};zero.vt=VT_I8;zero.hVal.QuadPart=0;
            check(events->QueueEventParamVar(MESourceStarted,GUID_NULL,S_OK,&zero));started=true;
            return;
        }
        ComPtr<IMFMediaTypeHandler> handler;ComPtr<IMFMediaType> type;check(sd->GetMediaTypeHandler(&handler));check(handler->GetCurrentMediaType(&type));
        ComPtr<IMFStreamDescriptor> current;check(stream->GetStreamDescriptor(&current));ComPtr<IMFMediaTypeHandler> ours;check(current->GetMediaTypeHandler(&ours));check(ours->IsMediaTypeSupported(type.Get(),nullptr));
        running=false;stream->select(type.Get());check(ours->SetCurrentMediaType(type.Get()));
        check(events->QueueEventParamUnk(started?MEUpdatedStream:MENewStream,GUID_NULL,S_OK,stream.Get()));
        stream->begin();PROPVARIANT zero{};zero.vt=VT_I8;zero.hVal.QuadPart=0;
        try{
            check(stream->events->QueueEventParamVar(MEStreamStarted,GUID_NULL,S_OK,&zero));
            check(events->QueueEventParamVar(MESourceStarted,GUID_NULL,S_OK,&zero));started=true;running=true;
            stream->releaseStart(); // no sample/error may overtake Start events
        }catch(...){stream->stop();throw;}
    });}
    STDMETHODIMP Stop() override{return guarded([&]{std::lock_guard lock(mutex);valid();running=false;if(stream){stream->stop();check(stream->events->QueueEventParamVar(MEStreamStopped,GUID_NULL,S_OK,nullptr));}check(events->QueueEventParamVar(MESourceStopped,GUID_NULL,S_OK,nullptr));});}
    STDMETHODIMP Pause() override{return MF_E_INVALID_STATE_TRANSITION;}
    STDMETHODIMP Shutdown() override{std::lock_guard lock(mutex);if(shutdown)return S_OK;shutdown=true;if(stream){stream->close();stream.Reset();}cameraActivation.reset();if(events)events->Shutdown();return S_OK;}
    STDMETHODIMP GetSourceAttributes(IMFAttributes** output) override{return attributes.CopyTo(output);}
    STDMETHODIMP GetStreamAttributes(DWORD id,IMFAttributes** output) override{return guarded([&]{std::lock_guard lock(mutex);ready();if(id)throw MF_E_INVALIDSTREAMNUMBER;check(stream->attributes.CopyTo(output));});}
    STDMETHODIMP SetD3DManager(IUnknown* supplied) override{return guarded([&]{std::lock_guard lock(mutex);valid();if(stream)stream->setManager(supplied);deviceManager=supplied;});}
    STDMETHODIMP GetService(REFGUID,REFIID,LPVOID* output) override{if(output)*output=nullptr;return MF_E_UNSUPPORTED_SERVICE;}
    STDMETHODIMP KsProperty(PKSPROPERTY p,ULONG pn,LPVOID data,ULONG n,ULONG* bytes) override{
        uint8_t target;
        {std::lock_guard lock(mutex);if(shutdown){if(bytes)*bytes=0;return MF_E_SHUTDOWN;}target=stream&&stream->captures()?property::ActiveSensor:static_cast<uint8_t>(sensor);}
        return properties.invoke(p,pn,data,n,bytes,target);
    }
    STDMETHODIMP KsMethod(PKSMETHOD,ULONG,LPVOID,ULONG,ULONG* bytes) override{return missing(bytes);}
    STDMETHODIMP KsEvent(PKSEVENT,ULONG,LPVOID,ULONG,ULONG* bytes) override{return missing(bytes);}
    STDMETHODIMP SetDefaultAllocator(DWORD id,IUnknown*) override{return id?MF_E_INVALIDSTREAMNUMBER:S_OK;}
    STDMETHODIMP GetAllocatorUsage(DWORD id,DWORD* input,MFSampleAllocatorUsage* usage) override{if(!input||!usage)return E_POINTER;if(id)return MF_E_INVALIDSTREAMNUMBER;*input=0;*usage=MFSampleAllocatorUsage_UsesCustomAllocator;return S_OK;}
};

class Activate final : public ComObject<Microsoft::WRL::ChainInterfaces<IMFActivate,IMFAttributes>>,public Counted {
    ComPtr<IMFAttributes> attributes;std::mutex mutex;selection::Sensor sensor=selection::Sensor::Rear;
public:
    HRESULT RuntimeClassInitialize(selection::Sensor selected){
        sensor=selected;HRESULT hr=MFCreateAttributes(&attributes,8);
        return hr;
    }
    S7_ATTRIBUTES(attributes)
    STDMETHODIMP ActivateObject(REFIID id,void** out) override{return guarded([&]{
        if(!out)throw E_POINTER;*out=nullptr;std::lock_guard lock(mutex);
        auto next=Make<Source>();if(!next)throw E_OUTOFMEMORY;
        try{next->initialize(attributes.Get(),sensor);check(next->QueryInterface(id,out));}
        catch(...){next->Shutdown();throw;}
        cameraTrace("fresh virtual source activation",unsigned(sensor),S_OK);
    });}
    // FrameServer owns each returned source and calls its Shutdown. An old
    // activation release must never shut down a newer endpoint's source.
    STDMETHODIMP ShutdownObject() override{return S_OK;}
    STDMETHODIMP DetachObject() override{return S_OK;}
    // The returned source owns its lifetime independently of this activation.
};
class Factory final : public ComObject<IClassFactory>,public Counted {
public:
 selection::Sensor sensor=selection::Sensor::Rear;
    STDMETHODIMP CreateInstance(IUnknown* outer,REFIID id,void** out) override{
        if(!out)return E_POINTER;*out=nullptr;if(outer)return CLASS_E_NOAGGREGATION;
        return guarded([&]{
#ifdef S7_LEGACY_FRAME_SERVER
            // Frame Server CoCreates the media source, not IMFVirtualCamera or a
            // desktop-only DirectShow filter. No registration action runs here.
            if(id!=__uuidof(IMFActivate)){
                auto next=Make<Source>();if(!next)throw E_OUTOFMEMORY;
                ComPtr<IUnknown> result;
                check(next->QueryInterface(id,reinterpret_cast<void**>(result.GetAddressOf())));
                try{next->initialize(nullptr,sensor);}catch(...){next->Shutdown();throw;}
                *out=result.Detach();return;
            }
#endif
            auto next=Make<Activate>();if(!next)throw E_OUTOFMEMORY;
            check(next->RuntimeClassInitialize(sensor));check(next->QueryInterface(id,out));
        });
    }
    STDMETHODIMP LockServer(BOOL lock) override{if(lock)++locks;else --locks;return S_OK;}
};
}
STDAPI DllGetClassObject(REFCLSID cls,REFIID id,void** out){
    if(!out)return E_POINTER;*out=nullptr;if(cls!=s7camera::ClassId&&cls!=s7camera::FrontClassId)return CLASS_E_CLASSNOTAVAILABLE;
    return s7camera::guarded([&]{auto factory=s7camera::Make<s7camera::Factory>();if(!factory)throw E_OUTOFMEMORY;factory->sensor=cls==s7camera::FrontClassId?s7camera::selection::Sensor::Front:s7camera::selection::Sensor::Rear;s7camera::check(factory->QueryInterface(id,out));});
}
STDAPI DllCanUnloadNow(){s7camera::featureLease().collect();
#ifndef S7_LEGACY_FRAME_SERVER
s7camera::cameraUsbLease().collect();
#endif
return s7camera::objects==0&&s7camera::locks==0?S_OK:S_FALSE;}
BOOL WINAPI DllMain(HINSTANCE,DWORD,LPVOID){return TRUE;}
