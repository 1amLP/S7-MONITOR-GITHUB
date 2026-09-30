#pragma once
#include "HostWindows.h"
#include "Protocol.h"
#include "EncoderRecovery.h"
#include <mfapi.h>
#include <mfidl.h>
#include <mftransform.h>
#include <mferror.h>
#include <codecapi.h>
#include <strmif.h>
#include <functional>
namespace s7 {
// Owned by the display device, not by each reconnecting codec session.
class MediaFoundation {
public:
    MediaFoundation(){check(MFStartup(MF_VERSION),"Media Foundation startup");}
    ~MediaFoundation(){const HRESULT hr=MFShutdown();if(FAILED(hr))log("Media Foundation shutdown",hr);}
    MediaFoundation(const MediaFoundation&)=delete;MediaFoundation& operator=(const MediaFoundation&)=delete;
};
struct EncoderLifetime;
class Encoder {
    std::shared_ptr<EncoderLifetime> lifetime_;
    ComPtr<IMFActivate> activation_;
    ComPtr<IMFTransform> transform_;
    ComPtr<IMFMediaEventGenerator> events_;
    ComPtr<ICodecAPI> codec_;
    ComPtr<IMFShutdown> shutdown_;
    ComPtr<IMFDXGIDeviceManager> manager_;
    bool gpuInput_=false;
    AnnexB annex_;
    Config config_;
    DWORD input_=0,output_=0;
    unsigned credits_=0,inFlight_=0;
    bool streaming_=false;
    bool requestRecoveryIDR_=false;
    unsigned emptyRun_=0;
    uint64_t emptyOutputs_=0,reportedInput_=0,reportedOutput_=0,lastReport_=0;
    uint64_t lastProgress_=0,lastPTS_=0;
    uint64_t inputCount_=0,outputCount_=0,firstInput_=0,lastInput_=0,firstOutput_=0,lastOutput_=0;
    uint64_t submitUS_=0,outputUS_=0,usbUS_=0,maxSubmitUS_=0,maxOutputUS_=0,maxUSBUS_=0;
    std::function<void(Bytes,uint64_t,bool)> sink_;
    void open(IMFActivate* activation,bool gpu);
    bool close()noexcept;
    void drain();
    void header();
    void output(bool discard=false);
    void property(const GUID& id,uint32_t value,bool required);
    void submitSample(IMFSample* sample,uint64_t pts,bool forceIDR,uint64_t started);
public:
    Encoder(LUID adapter,Config config,std::function<void(Bytes,uint64_t,bool)> sink,IMFDXGIDeviceManager* manager=nullptr);
    ~Encoder(){close();}
    Encoder(const Encoder&)=delete;Encoder& operator=(const Encoder&)=delete;
    void pump();
    bool ready()const{return credits_>0&&inFlight_<2;}
    void submit(const Bytes& nv12,uint64_t pts,bool forceIDR);
    void submitGPU(IMFSample* allocation,uint64_t pts,bool forceIDR);
    bool gpuInput()const{return gpuInput_;}
    const Config& config()const{return config_;}
};
}
