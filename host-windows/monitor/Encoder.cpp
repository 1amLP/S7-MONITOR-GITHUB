#include "Encoder.h"
#include "IoLease.h"
#include "MonitorTrace.h"
#include <algorithm>
#include <atomic>
#include <limits>
namespace s7 {
struct EncoderLifetime {
    MediaFoundation runtime;
    ComPtr<IMFActivate> activation;
    ComPtr<IMFTransform> transform;
    ComPtr<IMFShutdown> shutdown;
    ComPtr<IMFMediaEventGenerator> events;
    ComPtr<ICodecAPI> codec;
    std::atomic<bool> released{false};
    bool complete=false;
    bool releasable(){
        if(!released.load())return false;
        if(complete)return true;
        MFSHUTDOWN_STATUS status=MFSHUTDOWN_INITIATED;
        return shutdown&&SUCCEEDED(shutdown->GetShutdownStatus(&status))&&status==MFSHUTDOWN_COMPLETED;
    }
};
static IoLease<EncoderLifetime>& encoderLease(){static auto* value=new IoLease<EncoderLifetime>();return *value;}
static ComPtr<IMFMediaType> mediaType(GUID subtype,const Config& c){
    ComPtr<IMFMediaType> type;check(MFCreateMediaType(&type),"Create H.264 media type");
    check(type->SetGUID(MF_MT_MAJOR_TYPE,MFMediaType_Video),"Video type");check(type->SetGUID(MF_MT_SUBTYPE,subtype),"Video subtype");
    check(MFSetAttributeSize(type.Get(),MF_MT_FRAME_SIZE,c.width,c.height),"Video dimensions");
    check(MFSetAttributeRatio(type.Get(),MF_MT_FRAME_RATE,c.fps,1),"Video frame rate");check(MFSetAttributeRatio(type.Get(),MF_MT_PIXEL_ASPECT_RATIO,1,1),"Pixel aspect");
    check(type->SetUINT32(MF_MT_INTERLACE_MODE,MFVideoInterlace_Progressive),"Progressive video");
    check(type->SetUINT32(MF_MT_VIDEO_PRIMARIES,MFVideoPrimaries_BT709),"BT.709 primaries");
    check(type->SetUINT32(MF_MT_TRANSFER_FUNCTION,MFVideoTransFunc_709),"BT.709 transfer");
    check(type->SetUINT32(MF_MT_YUV_MATRIX,MFVideoTransferMatrix_BT709),"BT.709 matrix");
    check(type->SetUINT32(MF_MT_VIDEO_NOMINAL_RANGE,MFNominalRange_16_235),"Video range");return type;
}
Encoder::Encoder(LUID adapter,Config config,std::function<void(Bytes,uint64_t,bool)> sink):config_(config),sink_(std::move(sink)){
    try{
        ComPtr<IMFAttributes> filter;check(MFCreateAttributes(&filter,1),"MFT adapter filter");
        check(filter->SetBlob(MFT_ENUM_ADAPTER_LUID,reinterpret_cast<UINT8*>(&adapter),sizeof(adapter)),"MFT adapter LUID");
        MFT_REGISTER_TYPE_INFO input{MFMediaType_Video,MFVideoFormat_NV12},output{MFMediaType_Video,MFVideoFormat_H264};
        IMFActivate** list=nullptr;UINT32 count=0;
        check(MFTEnum2(MFT_CATEGORY_VIDEO_ENCODER,MFT_ENUM_FLAG_HARDWARE|MFT_ENUM_FLAG_SORTANDFILTER,&input,&output,filter.Get(),&list,&count),"Enumerate hardware H.264 encoders on render GPU");
        struct List {IMFActivate** p;UINT32 count;~List(){for(UINT32 i=0;i<count;i++)p[i]->Release();CoTaskMemFree(p);}}cleanup{list,count};
        HRESULT last=MF_E_TOPO_CODEC_NOT_FOUND;
        for(UINT32 i=0;i<count;i++){
            auto owner=std::make_shared<EncoderLifetime>();
            const auto deadline=GetTickCount64()+750;
            while(!encoderLease().acquire(owner)){
                if(GetTickCount64()>=deadline)throw Failure(HRESULT_FROM_WIN32(ERROR_BUSY),"Previous encoder shutdown is incomplete");
                Sleep(2);
            }
            lifetime_=owner;
            try{open(list[i]);lastProgress_=microseconds();return;}
            catch(const Failure& e){last=e.code;monitorEvent(e.what(),e.code);if(!close())throw Failure(HRESULT_FROM_WIN32(ERROR_IO_INCOMPLETE),"Hardware encoder cleanup retained its owner");}
        }
        throw Failure(last,"No hardware H.264 encoder accepted low-latency NV12 mode (no software fallback)");
    }catch(...){close();throw;}
}
void Encoder::property(const GUID& id,uint32_t value,bool required){
    if(codec_->IsSupported(&id)!=S_OK){if(required)throw Failure(E_NOTIMPL,"Required encoder control unavailable");return;}
    VARIANT v;VariantInit(&v);v.vt=VT_UI4;v.ulVal=value;HRESULT hr=codec_->SetValue(&id,&v);
    if(required)check(hr,"Apply encoder control");else if(FAILED(hr))log("Optional encoder control rejected",hr);
}
void Encoder::open(IMFActivate* activation){
    activation_=activation;
    check(activation->ActivateObject(IID_PPV_ARGS(&transform_)),"Activate hardware encoder");
    ComPtr<IMFAttributes> attr;check(transform_->GetAttributes(&attr),"Encoder attributes");UINT32 async=0;check(attr->GetUINT32(MF_TRANSFORM_ASYNC,&async),"Hardware MFT asynchronous flag");
    if(!async)throw Failure(E_NOTIMPL,"Synchronous encoder was not accepted as hardware");
    check(attr->SetUINT32(MF_TRANSFORM_ASYNC_UNLOCK,TRUE),"Unlock asynchronous MFT");
    check(attr->SetUINT32(MF_LOW_LATENCY,TRUE),"Encoder low latency attribute");
    check(transform_.As(&events_),"MFT event generator");check(transform_.As(&codec_),"Encoder codec controls");
    check(transform_.As(&shutdown_),"Asynchronous encoder shutdown interface");
    DWORD inCount=0,outCount=0;check(transform_->GetStreamCount(&inCount,&outCount),"Encoder stream count");if(inCount!=1||outCount!=1)throw Failure(E_NOTIMPL,"Unexpected encoder streams");
    HRESULT ids=transform_->GetStreamIDs(1,&input_,1,&output_);if(ids==E_NOTIMPL){input_=output_=0;}else check(ids,"Encoder stream IDs");
    property(CODECAPI_AVEncCommonRateControlMode,eAVEncCommonRateControlMode_CBR,true);
    property(CODECAPI_AVEncCommonMeanBitRate,config_.bitrate,true);
    property(CODECAPI_AVEncMPVDefaultBPictureCount,0,false);
    property(CODECAPI_AVEncMPVGOPSize,config_.fps*config_.gopSeconds,true);
    if(codec_->IsSupported(&CODECAPI_AVLowLatencyMode)==S_OK){VARIANT v;VariantInit(&v);v.vt=VT_BOOL;v.boolVal=VARIANT_TRUE;check(codec_->SetValue(&CODECAPI_AVLowLatencyMode,&v),"Low latency codec mode");}
    if(codec_->IsSupported(&CODECAPI_AVEncVideoForceKeyFrame)!=S_OK)throw Failure(E_NOTIMPL,"Encoder cannot request IDR");
    auto out=mediaType(MFVideoFormat_H264,config_);
    check(out->SetUINT32(MF_MT_AVG_BITRATE,config_.bitrate),"AVC bitrate");
    check(out->SetUINT32(MF_MT_MPEG2_PROFILE,eAVEncH264VProfile_Base),"H.264 Baseline (no B frames)");
    // The encoder derives a level from resolution, frame rate AND bitrate.
    // A fixed level 3.1/3.2 cannot represent the whole 2..30 Mbit/s menu range.
    // See Microsoft H.264 Video Encoder documentation, MF_MT_MPEG2_LEVEL.

    check(transform_->SetOutputType(output_,out.Get(),0),"Encoder output type");
    auto in=mediaType(MFVideoFormat_NV12,config_);check(in->SetUINT32(MF_MT_DEFAULT_STRIDE,config_.width),"NV12 stride");
    check(transform_->SetInputType(input_,in.Get(),0),"Hardware encoder NV12 memory input");
    check(transform_->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING,0),"Begin encoder streaming");streaming_=true;
    check(transform_->ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM,0),"Start encoder stream");header();
}
void Encoder::header(){
    annex_.reset();
    ComPtr<IMFMediaType> type;check(transform_->GetOutputCurrentType(output_,&type),"Current H.264 output type");UINT32 size=0;
    HRESULT hr=type->GetBlobSize(MF_MT_MPEG_SEQUENCE_HEADER,&size);if(hr==MF_E_ATTRIBUTENOTFOUND)return;check(hr,"AVC sequence header size");
    if(size==0)return;if(size>MaxAccessUnit)throw Failure(E_INVALIDARG,"AVC sequence header too large");
    Bytes b(size);check(type->GetBlob(MF_MT_MPEG_SEQUENCE_HEADER,b.data(),size,&size),"AVC sequence header");annex_.config(b.data(),size);
}
void Encoder::submit(const Bytes& nv12,uint64_t pts,bool forceIDR){
    const auto started=microseconds();
    if(!ready()||nv12.size()!=size_t(config_.width)*config_.height*3/2)throw Failure(E_INVALIDARG,"Encoder input state/size");
    if(forceIDR||requestRecoveryIDR_){property(CODECAPI_AVEncVideoForceKeyFrame,1,true);requestRecoveryIDR_=false;}
    ComPtr<IMFSample> sample;ComPtr<IMFMediaBuffer> buffer;
    check(MFCreateSample(&sample),"Encoder input sample");check(MFCreateMemoryBuffer(DWORD(nv12.size()),&buffer),"Encoder input buffer");
    BYTE* p=nullptr;check(buffer->Lock(&p,nullptr,nullptr),"Lock NV12 input");std::memcpy(p,nv12.data(),nv12.size());check(buffer->Unlock(),"Unlock NV12 input");
    check(buffer->SetCurrentLength(DWORD(nv12.size())),"NV12 input length");check(sample->AddBuffer(buffer.Get()),"NV12 sample buffer");
    if(pts>uint64_t(INT64_MAX/10))throw Failure(E_INVALIDARG,"Encoder timestamp overflow");
    check(sample->SetSampleTime(LONGLONG(pts*10)),"Encoder sample timestamp");check(sample->SetSampleDuration(10000000/config_.fps),"Encoder sample duration");
    check(transform_->ProcessInput(input_,sample.Get(),0),"Hardware encode input");credits_--;if(inFlight_++==0)lastProgress_=microseconds();
    const auto finished=microseconds();
    if(!inputCount_)firstInput_=started;++inputCount_;lastInput_=started;
    submitUS_+=finished-started;maxSubmitUS_=std::max(maxSubmitUS_,finished-started);
}
void Encoder::output(bool discard){
    const auto started=microseconds();
    MFT_OUTPUT_STREAM_INFO info{};check(transform_->GetOutputStreamInfo(output_,&info),"Encoder output stream info");
    ComPtr<IMFSample> allocated;
    if(!(info.dwFlags&(MFT_OUTPUT_STREAM_PROVIDES_SAMPLES|MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES))){
        if(info.cbSize>MaxAccessUnit)throw Failure(E_INVALIDARG,"Encoder output allocation exceeds 1 MiB");
        ComPtr<IMFMediaBuffer> buffer;check(MFCreateAlignedMemoryBuffer(DWORD(MaxAccessUnit),info.cbAlignment,&buffer),"Output buffer");check(MFCreateSample(&allocated),"Output sample");check(allocated->AddBuffer(buffer.Get()),"Output sample buffer");
    }
    MFT_OUTPUT_DATA_BUFFER out{};out.dwStreamID=output_;out.pSample=allocated.Get();DWORD state=0;
    HRESULT hr=transform_->ProcessOutput(0,1,&out,&state);ComPtr<IMFCollection> eventCollection;eventCollection.Attach(out.pEvents);
    ComPtr<IMFSample> sample;if(out.pSample==allocated.Get())sample=allocated;else sample.Attach(out.pSample);
    if(hr==MF_E_TRANSFORM_STREAM_CHANGE){ComPtr<IMFMediaType> type;check(transform_->GetOutputAvailableType(output_,0,&type),"Changed AVC output type");check(transform_->SetOutputType(output_,type.Get(),0),"Accept AVC output type change");header();return;}
    check(hr,"Hardware encoder output");if(!sample)throw Failure(E_FAIL,"Encoder output event without sample");
    if(discard){if(inFlight_)--inFlight_;return;}
    ComPtr<IMFMediaBuffer> buffer;check(sample->ConvertToContiguousBuffer(&buffer),"Contiguous AVC output");BYTE* data=nullptr;DWORD length=0;check(buffer->Lock(&data,nullptr,&length),"Lock AVC output");
    const auto size=encodedSize(length,MaxAccessUnit);
    if(size!=EncodedSize::Picture){
        check(buffer->Unlock(),"Unlock empty/invalid AVC output");
        char detail[192]{};
        sprintf_s(detail,"Encoder output bytes=%lu max=%zu flags=0x%lx gen=%u",(unsigned long)length,MaxAccessUnit,(unsigned long)out.dwStatus,config_.generation);
        monitorEvent(detail,size==EncodedSize::Empty?S_OK:MF_E_INVALID_STREAM_DATA);
        if(size==EncodedSize::Invalid)throw Failure(MF_E_INVALID_STREAM_DATA,"Encoder output violates transport size bounds");
        // A skipped input is not a video frame. Do not send or count an empty
        // sample, and restart prediction with the next independently coded frame.
        if(inFlight_)--inFlight_;
        ++emptyOutputs_;requestRecoveryIDR_=true;
        if(++emptyRun_>=3)throw Failure(MF_E_INVALID_STREAM_DATA,"Encoder returned three consecutive empty samples");
        return;
    }
    Bytes packet;bool idr=false;
    try{packet=annex_.sample(data,length,idr);}
    catch(const std::runtime_error& e){buffer->Unlock();throw Failure(MF_E_INVALID_STREAM_DATA,e.what());}
    catch(...){buffer->Unlock();throw;}
    check(buffer->Unlock(),"Unlock AVC output");emptyRun_=0;
    LONGLONG pts=0;check(sample->GetSampleTime(&pts),"AVC output timestamp");
    if(pts<0||uint64_t(pts/10)<=lastPTS_)throw Failure(E_FAIL,"Encoder reordered/duplicated timestamps");lastPTS_=uint64_t(pts/10);
    if(inFlight_)inFlight_--;lastProgress_=microseconds();
    const auto ready=microseconds();
    if(!outputCount_)firstOutput_=ready;++outputCount_;lastOutput_=ready;
    outputUS_+=ready-started;maxOutputUS_=std::max(maxOutputUS_,ready-started);
    sink_(std::move(packet),lastPTS_,idr);
    const auto sent=microseconds();usbUS_+=sent-ready;maxUSBUS_=std::max(maxUSBUS_,sent-ready);
}
void Encoder::pump(){
    for(unsigned count=0;count<64;count++){
        ComPtr<IMFMediaEvent> event;HRESULT hr=events_->GetEvent(MF_EVENT_FLAG_NO_WAIT,&event);
        if(hr==MF_E_NO_EVENTS_AVAILABLE)break;check(hr,"Poll encoder event");HRESULT status=S_OK;check(event->GetStatus(&status),"MFT event status");check(status,"Hardware encoder event failure");MediaEventType type;check(event->GetType(&type),"MFT event type");
        if(type==METransformNeedInput){if(credits_==std::numeric_limits<unsigned>::max())throw Failure(E_FAIL,"Encoder input-credit overflow");credits_++;}
        else if(type==METransformHaveOutput)output();
    }
    if(inFlight_&&microseconds()-lastProgress_>1000000)throw Failure(HRESULT_FROM_WIN32(ERROR_TIMEOUT),"Hardware encoder stalled for one second");
    const auto now=microseconds();
    if(!lastReport_)lastReport_=now;
    if(now-lastReport_>=5000000){
        char detail[320]{};
        sprintf_s(detail,"Encoder cadence gen=%u window_us=%llu input=%llu output=%llu empty_total=%llu inflight=%u credits=%u",
            config_.generation,(unsigned long long)(now-lastReport_),
            (unsigned long long)(inputCount_-reportedInput_),(unsigned long long)(outputCount_-reportedOutput_),
            (unsigned long long)emptyOutputs_,inFlight_,credits_);
        monitorEvent(detail);lastReport_=now;reportedInput_=inputCount_;reportedOutput_=outputCount_;
    }
}
void Encoder::drain(){
    check(transform_->ProcessMessage(MFT_MESSAGE_NOTIFY_END_OF_STREAM,input_),"End encoder input");
    check(transform_->ProcessMessage(MFT_MESSAGE_COMMAND_DRAIN,input_),"Drain hardware encoder");
    const auto deadline=GetTickCount64()+750;
    while(GetTickCount64()<deadline){
        ComPtr<IMFMediaEvent> event;const HRESULT hr=events_->GetEvent(MF_EVENT_FLAG_NO_WAIT,&event);
        if(hr==MF_E_NO_EVENTS_AVAILABLE){Sleep(1);continue;}
        check(hr,"Read encoder drain event");HRESULT status=S_OK;check(event->GetStatus(&status),"Encoder drain status");check(status,"Encoder drain failure");
        MediaEventType type;check(event->GetType(&type),"Encoder drain event type");
        if(type==METransformHaveOutput)output(true);
        else if(type==METransformDrainComplete)return;
    }
    throw Failure(HRESULT_FROM_WIN32(ERROR_TIMEOUT),"Encoder drain did not complete");
}
bool Encoder::close()noexcept{
    if(!lifetime_)return true;
    if(transform_&&streaming_){
        try{drain();check(transform_->ProcessMessage(MFT_MESSAGE_NOTIFY_END_STREAMING,0),"End encoder streaming");}
        catch(const Failure& e){monitorEvent(e.what(),e.code);}
        catch(...){monitorEvent("Encoder drain exception",E_FAIL);}
    }
    bool complete=!transform_;
    if(shutdown_){
        const HRESULT hr=shutdown_->Shutdown();
        if(FAILED(hr)&&hr!=MF_E_SHUTDOWN)monitorEvent("Encoder IMFShutdown",hr);
        MFSHUTDOWN_STATUS status=MFSHUTDOWN_INITIATED;
        complete=SUCCEEDED(shutdown_->GetShutdownStatus(&status))&&status==MFSHUTDOWN_COMPLETED;
    }else if(activation_&&!streaming_){
        complete=SUCCEEDED(activation_->ShutdownObject());
    }
    auto owner=lifetime_;
    owner->activation=activation_;owner->transform=transform_;owner->shutdown=shutdown_;owner->events=events_;owner->codec=codec_;owner->complete=complete;
    owner->released.store(true);
    codec_.Reset();events_.Reset();shutdown_.Reset();transform_.Reset();activation_.Reset();streaming_=false;
    char stats[640]{};
    _snprintf_s(stats,sizeof(stats),_TRUNCATE,"Encoder gen=%u %ux%u input=%llu output=%llu input_span_us=%llu output_span_us=%llu submit_mean_us=%llu output_mean_us=%llu usb_mean_us=%llu usb_max_us=%llu shutdown_complete=%u",
        config_.generation,config_.width,config_.height,(unsigned long long)inputCount_,(unsigned long long)outputCount_,
        (unsigned long long)(lastInput_-firstInput_),(unsigned long long)(lastOutput_-firstOutput_),
        (unsigned long long)(inputCount_?submitUS_/inputCount_:0),(unsigned long long)(outputCount_?outputUS_/outputCount_:0),
        (unsigned long long)(outputCount_?usbUS_/outputCount_:0),(unsigned long long)maxUSBUS_,complete?1u:0u);
    monitorEvent(stats,complete?S_OK:HRESULT_FROM_WIN32(ERROR_IO_INCOMPLETE));
    lifetime_.reset();encoderLease().collect();
    return complete;
}
}
