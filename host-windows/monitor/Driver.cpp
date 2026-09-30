// SPDX-License-Identifier: GPL-3.0-or-later
// UMDF2/IddCx software display. Only this monitor's swap chain is encoded.
#include "HostWindows.h"
#include <bugcodes.h>
#include <wudfwdm.h>
#include <wdf.h>
#include <iddcx.h>
#include <dxgi1_5.h>
#include <d3d11.h>
#include <avrt.h>
#include <atomic>
#include <thread>
#include <mutex>
#include <new>
#include "Usb.h"
#include "MonitorDescription.h"
#include "Encoder.h"
#include "MonitorTrace.h"
#include "DesktopReadback.h"
#include "GpuFrame.h"
#include "PresencePolicy.h"
#include "FrameHandoff.h"
#include "MonitorSlot.h"
#include "SniperCadence.h"
namespace s7 {
static void ntcheck(NTSTATUS status,const char* where){if(!NT_SUCCESS(status))throw Failure(HRESULT_FROM_NT(status),where);}
static bool signaled(HANDLE event){return WaitForSingleObject(event,0)==WAIT_OBJECT_0;}
static Handle event(){HANDLE h=CreateEventW(nullptr,TRUE,FALSE,nullptr);wincheck(h!=nullptr,"Create stop event");return Handle(h);}
static Handle pictureEvent(){HANDLE h=CreateEventW(nullptr,FALSE,FALSE,nullptr);wincheck(h!=nullptr,"Create picture event");return Handle(h);}
struct Session {
    struct Snapshot {std::shared_ptr<Usb> usb;Config current;uint64_t epoch;};
    std::mutex mutex;
    std::shared_ptr<Usb> transport;
    Config current;
    uint64_t epoch=1;
    std::atomic<HRESULT> failure{S_OK},transportFailure{S_OK};
    Session(std::shared_ptr<Usb> u,Config c):transport(std::move(u)),current(c){}
    Snapshot snapshot(){std::lock_guard<std::mutex> lock(mutex);return {transport,current,epoch};}
    void update(const std::shared_ptr<Usb>& u,Config c){
        std::lock_guard<std::mutex> lock(mutex);
        if(current.generation!=c.generation)failure=S_OK;
        if(transport!=u){transport=u;++epoch;transportFailure=S_OK;}
        else if(current.consumer!=c.consumer||current.enabled!=c.enabled){++epoch;}
        current=c;
    }
    void failedEncoding(const std::shared_ptr<Usb>& u,uint64_t expectedEpoch,uint32_t generation,HRESULT error){
        std::lock_guard<std::mutex> lock(mutex);
        if(transport==u&&epoch==expectedEpoch&&current.generation==generation)failure=error;
    }
    void suspend(){std::lock_guard<std::mutex> lock(mutex);if(transport){transport.reset();++epoch;}transportFailure=S_OK;}
    void failedTransport(const std::shared_ptr<Usb>& u,HRESULT error){
        std::lock_guard<std::mutex> lock(mutex);if(transport==u)transportFailure=error;
    }
};
class Worker {
    IDDCX_SWAPCHAIN chain_;
    LUID adapter_;
    HANDLE available_;
    std::shared_ptr<Session> session_;
    Handle stop_=event();
    Handle pictureReady_=pictureEvent();
    FrameHandoff pictures_;
    uint64_t captureFirst_=0,captureLast_=0,captureCount_=0,captureUS_=0,captureMaxUS_=0;
    uint64_t gpuFrames_=0,gpuPoolWaits_=0;
    std::atomic<bool> gpuInput_{false};
    std::thread thread_; // Started last: every field touched by run() is initialized.
    static StreamKey streamKey(const Session::Snapshot& s){
        return {s.epoch,s.current.generation,s.current.fps,s.current.bitrate,s.current.gopSeconds,s.current.width,s.current.height};
    }
    bool active(const Session::Snapshot& s,uint32_t fps)const {
        return s.usb&&s.current.enabled&&s.current.consumer&&s.current.fps==fps&&
               SUCCEEDED(session_->failure.load())&&SUCCEEDED(session_->transportFailure.load());
    }
    // All MFT activation/pumping/submission/teardown and USB sends run here.
    // No IddCx surface or D3D immediate context crosses this thread boundary.
    void transmit(IMFDXGIDeviceManager* manager)noexcept{
        try{
            ComRuntime com;
            Handle timer(CreateWaitableTimerExW(nullptr,nullptr,0x2,TIMER_MODIFY_STATE|SYNCHRONIZE));
            wincheck(timer.get()!=nullptr,"Create high-resolution frame timer");
            const auto fps=session_->snapshot().current.fps;
            StreamKey applied{};Config config{};uint32_t keyRequest=0;
            uint64_t nextFrame=0,lastSubmit=0,lastPTS=0;bool forceIDR=true,newPixels=false;
            DesktopPicture current;
            std::unique_ptr<Encoder> encoder;
            EncoderRetryBudget retries;
            uint64_t retryAt=0;
            while(!signaled(stop_.get())){
                const auto snapshot=session_->snapshot();const auto key=streamKey(snapshot);
                if(!active(snapshot,fps)){
                    if(encoder){
                        const HRESULT failure=session_->failure.load(),transport=session_->transportFailure.load();
                        char detail[256]{};
                        _snprintf_s(detail,sizeof(detail),_TRUNCATE,
                            "Monitor video paused gen=%u usb=%u enabled=%u consumer=%u fps=%u worker_fps=%u encoder_hr=0x%08lx transport_hr=0x%08lx",
                            snapshot.current.generation,snapshot.usb?1u:0u,snapshot.current.enabled?1u:0u,snapshot.current.consumer?1u:0u,
                            snapshot.current.fps,fps,static_cast<unsigned long>(failure),static_cast<unsigned long>(transport));
                        monitorEvent(detail,FAILED(transport)?transport:failure);
                    }
                    gpuInput_=false;pictures_.pause();encoder.reset();current=DesktopPicture{};applied={};
                }else if(microseconds()>=retryAt)try{
                    if(!encoder||applied!=key){
                        pictures_.pause();encoder.reset();current=DesktopPicture{};
                        applied=key;config=snapshot.current;keyRequest=config.keyRequest;
                        forceIDR=true;newPixels=false;nextFrame=lastSubmit=lastPTS=0;
                        auto usb=snapshot.usb;
                        encoder=std::make_unique<Encoder>(adapter_,config,[this,usb,config,key,ack=false](Bytes bytes,uint64_t pts,bool idr)mutable{
                            const auto live=session_->snapshot();
                            if(signaled(stop_.get())||!active(live,config.fps)||streamKey(live)!=key)return;
                            usb->send(config,bytes,pts,idr,stop_.get());
                            if(!ack){usb->status(config,2,S_OK,stop_.get());ack=true;}
                        },config.sniper?nullptr:manager);
                        gpuInput_=encoder->gpuInput();
                        if(!pictures_.configure(key))throw Failure(E_INVALIDARG,"Invalid monitor stream identity");
                        usb->status(config,1,S_OK,stop_.get());
                    }
                    if(keyRequest!=snapshot.current.keyRequest){keyRequest=snapshot.current.keyRequest;forceIDR=true;}
                    encoder->pump(); // May block in vendor MFT or USB; never blocks the acquisition loop.
                    const auto latest=session_->snapshot();
                    if(!active(latest,fps)||streamKey(latest)!=applied)continue;
                    auto now=microseconds();
                    // DWM/Sniper already produces at the selected display rate.
                    // A second wall-clock gate coalesces fresh frames around the
                    // deadline when MFT completion/readback have different phases.
                    if(encoder->ready()){
                        newPixels=pictures_.take(applied,now,current,forceIDR)||newPixels;
                        // A static-desktop refresh/IDR reuses pixels deliberately. Only a
                        // successful take is a NEW desktop picture, not the repeated sample.
                        if((current.gpu||!current.pixels.empty())&&(newPixels||forceIDR||now-lastSubmit>=1000000)){
                            const auto pts=samplePTS(newPixels,current.acquiredUS,now,lastPTS);
                            if(pts){
                                if(current.gpu)encoder->submitGPU(current.gpu->allocation.Get(),pts,forceIDR);
                                else encoder->submit(current.pixels,pts,forceIDR);
                                forceIDR=false;newPixels=false;lastPTS=pts;pictures_.submitted(pts);lastSubmit=now;
                                const uint64_t period=1000000/config.fps;
                                nextFrame=(!nextFrame||now>=nextFrame+period)?now+period:nextFrame+period;
                            }
                        }
                    }
                }catch(const TransportFailure& e){
                    const bool stopping=signaled(stop_.get());
                    if(!stopping)session_->failedTransport(snapshot.usb,e.code);
                    if(!stopping||e.code!=HRESULT_FROM_WIN32(ERROR_CANCELLED))monitorEvent(e.what(),e.code);
                    pictures_.pause();encoder.reset();applied={};current=DesktopPicture{};
                }catch(const Failure& e){
                    if(!signaled(stop_.get())){
                        monitorEvent(e.what(),e.code);
                        if(retries.take(microseconds())){
                            retryAt=microseconds()+250000;
                            monitorEvent("Retry monitor encoder without changing USB or display",e.code);
                        }else session_->failedEncoding(snapshot.usb,snapshot.epoch,snapshot.current.generation,e.code);
                    }
                    pictures_.pause();encoder.reset();applied={};current=DesktopPicture{};
                }
                // A 2 ms ordinary wait can wake on the coarse Windows tick and
                // quantize 60 Hz into roughly 30 Hz. No global timer-policy change.
                const auto clockNow=microseconds();
                uint64_t delay=active(snapshot,fps)?2000:20000;
                if(nextFrame>clockNow)delay=std::min(delay,nextFrame-clockNow);
                LARGE_INTEGER due{};due.QuadPart=-static_cast<LONGLONG>(std::max<uint64_t>(100,delay)*10);
                wincheck(SetWaitableTimer(timer.get(),&due,0,nullptr,nullptr,FALSE),"Arm frame timer");
                HANDLE waits[]={stop_.get(),pictureReady_.get(),timer.get()};
                if(WaitForMultipleObjects(3,waits,FALSE,INFINITE)==WAIT_FAILED)throw Failure(HRESULT_FROM_WIN32(GetLastError()),"Wait for picture/codec cadence");
            }
        }catch(const std::exception& e){if(!signaled(stop_.get())){session_->failure=E_FAIL;monitorEvent(e.what(),E_FAIL);}}
        catch(...){if(!signaled(stop_.get())){session_->failure=E_FAIL;monitorEvent("Unknown monitor transmitter failure",E_FAIL);}}
        pictures_.pause();
    }
    void core(std::thread& sender){
        ComRuntime com;
        DWORD task=0;HANDLE av=AvSetMmThreadCharacteristicsW(L"Distribution",&task);
        struct AvGuard{HANDLE h;~AvGuard(){if(h)AvRevertMmThreadCharacteristics(h);}} priority{av};
        ComPtr<IDXGIFactory4> factory;ComPtr<IDXGIAdapter1> adapter;
        check(CreateDXGIFactory2(0,IID_PPV_ARGS(&factory)),"Create render GPU factory");
        check(factory->EnumAdapterByLuid(adapter_,IID_PPV_ARGS(&adapter)),"Find swap-chain render GPU");
        ComPtr<ID3D11Device> device;ComPtr<ID3D11DeviceContext> context;
        check(D3D11CreateDevice(adapter.Get(),D3D_DRIVER_TYPE_UNKNOWN,nullptr,D3D11_CREATE_DEVICE_BGRA_SUPPORT|D3D11_CREATE_DEVICE_VIDEO_SUPPORT,nullptr,0,D3D11_SDK_VERSION,&device,nullptr,&context),"Create render GPU device");
        ComPtr<IDXGIDevice> dxgi;check(device.As(&dxgi),"Render DXGI device");
        IDARG_IN_SWAPCHAINSETDEVICE bind{};bind.pDevice=dxgi.Get();check(IddCxSwapChainSetDevice(chain_,&bind),"Bind IddCx swap-chain device");
        const auto initial=session_->snapshot().current;
        DesktopReadback readback(device.Get(),context.Get(),initial.width,initial.height);
        std::shared_ptr<GpuFramePool> gpu;
        try{gpu=std::make_shared<GpuFramePool>(device.Get(),context.Get(),initial.width,initial.height);}
        catch(const Failure& e){monitorEvent("GPU surface pool unavailable; NV12 readback retained",e.code);}
        sender=std::thread([this,gpu]{transmit(gpu?gpu->manager():nullptr);});
        DesktopPicture capture;
        std::unique_ptr<SniperChannel> primary;
        DWORD primarySession=0xffffffff;
        uint64_t primarySequence=0,primaryRetry=0,primaryPoll=0;
        uint16_t inputAck=0;
        uint32_t primaryGeneration=0;
        bool copyPending=false,captureWaiting=false;
        Handle readbackTimer(CreateWaitableTimerExW(nullptr,nullptr,0x2,TIMER_MODIFY_STATE|SYNCHRONIZE));
        wincheck(bool(readbackTimer),"Create GPU readback timer");
        StreamKey copyKey{};
        uint64_t copyTime=0;
        ComPtr<ID3D11Texture2D> desktop;
        StreamKey desktopKey{};
        uint64_t desktopTime=0;
        LARGE_INTEGER qpcFrequency{};wincheck(QueryPerformanceFrequency(&qpcFrequency),"Capture QPC frequency");
        uint64_t windowStart=microseconds(),acquires=0,unique=0,missingIds=0,duplicates=0;
        uint64_t desiredFirst=0,desiredLast=0,acquireMaxUS=0,lateMaxUS=0;
        uint64_t reportedReadbacks=0,reportedReadbackUS=0,reportedGPUFrames=0,reportedGPUWaits=0;
        UINT lastFrameNumber=0;bool haveFrameNumber=false;
        while(!signaled(stop_.get())){
            const auto snapshot=session_->snapshot();const auto key=streamKey(snapshot);
            const bool streaming=active(snapshot,initial.fps);
            // Keep pending GPU copy ownership until Map succeeds; pause/generation
            // changes discard its RESULT, never overwrite an in-flight staging copy.
            // Even while paused/failed, drain the compositor's swap chain.
            // Transport recovery must not stall DWM or delete the monitor.
            auto now=microseconds();
            if(now-windowStart>=5000000){
                char sample[640]{};
                const uint64_t desiredSpan=desiredLast>=desiredFirst?(desiredLast-desiredFirst)*1000000/uint64_t(qpcFrequency.QuadPart):0;
                const auto readbacks=captureCount_-reportedReadbacks;
                _snprintf_s(sample,sizeof(sample),_TRUNCATE,"Capture mode=%s %ux%u window_us=%llu acquired=%llu unique_ids=%llu skipped_ids=%llu repeated_ids=%llu desired_span_us=%llu acquire_max_us=%llu late_max_us=%llu readbacks=%llu readback_mean_us=%llu",
                    snapshot.current.sniper?"Sniper":"Monitor",initial.width,initial.height,(unsigned long long)(now-windowStart),(unsigned long long)acquires,(unsigned long long)unique,
                    (unsigned long long)missingIds,(unsigned long long)duplicates,(unsigned long long)desiredSpan,
                    (unsigned long long)acquireMaxUS,(unsigned long long)lateMaxUS,
                    (unsigned long long)readbacks,(unsigned long long)(readbacks?(captureUS_-reportedReadbackUS)/readbacks:0));
                monitorEvent(sample);windowStart=now;acquires=unique=missingIds=duplicates=0;
                if(gpuInput_.load()){
                    sprintf_s(sample,"Monitor GPU frames=%llu allocator_waits=%llu CPU_NV12_readbacks=%llu",(unsigned long long)(gpuFrames_-reportedGPUFrames),
                        (unsigned long long)(gpuPoolWaits_-reportedGPUWaits),(unsigned long long)readbacks);
                    monitorEvent(sample);
                }
                reportedGPUFrames=gpuFrames_;reportedGPUWaits=gpuPoolWaits_;
                reportedReadbacks=captureCount_;reportedReadbackUS=captureUS_;
                desiredFirst=desiredLast=acquireMaxUS=lateMaxUS=0;
            }
            const bool sniper=streaming&&snapshot.current.sniper;
            if(!sniper&&primary){
                primary->access([](SniperShared& s){s.request.atMS=0;});
                primary.reset();primaryGeneration=0;inputAck=0;
            }
            if(sniper&&now>=primaryPoll){
                primaryPoll=sniperDeadline(primaryPoll,now);
                const DWORD console=WTSGetActiveConsoleSessionId();
                if(primary&&primarySession!=console){primary.reset();primaryGeneration=0;}
                if(!primary&&now>=primaryRetry){
                    primaryRetry=now+500000;
                    try{primary=std::make_unique<SniperChannel>(console);primarySession=console;primarySequence=0;}
                    catch(const Failure& e){log("Sniper user-session channel pending",e.code);}
                }
                if(primary){
                    if(primaryGeneration!=snapshot.current.generation){primaryGeneration=snapshot.current.generation;inputAck=0;primarySequence=0;}
                    SniperControl control;
                    try{control=snapshot.usb->sniper(stop_.get(),inputAck);}
                    catch(const TransportFailure& e){session_->failedTransport(snapshot.usb,e.code);continue;}
                    if(control.active&&control.generation==primaryGeneration){
                        HRESULT primaryError=S_OK;
                        primary->access([&](SniperShared& s){
                            const bool same=s.request.generation==primaryGeneration;
                            s.request={primaryGeneration,initial.width,initial.height,control.zoom,control.x,control.y,uint32_t(control.stretch),control.scaleX,control.scaleY,control.sequence,control.contactX,control.contactY,uint32_t(control.down),control.rotation,uint32_t(control.mirror),GetTickCount64()};
                            if(!same){s.acceptedSequence=0;s.error=S_OK;}
                            primaryError=s.error;
                            inputAck=static_cast<uint16_t>(s.acceptedSequence);
                        });
                        if(FAILED(primaryError))session_->failedEncoding(snapshot.usb,snapshot.epoch,primaryGeneration,primaryError);
                    }
                }
            }
            // Consume a ready shared picture between USB control polls. Two
            // independent 60Hz clocks otherwise discard frames at their edges.
            if(sniper&&primary){
                primary->access([&](SniperShared& s){
                    inputAck=static_cast<uint16_t>(s.acceptedSequence);
                    if(s.frameGeneration==primaryGeneration&&s.frameSequence!=primarySequence&&s.bytes==initial.width*initial.height*3/2&&s.bytes<=SniperMaxBytes){
                        ++acquires;++unique;
                        if(primarySequence&&s.frameSequence>primarySequence)missingIds+=s.frameSequence-primarySequence-1;
                        primarySequence=s.frameSequence;
                        capture.key=key;capture.acquiredUS=s.frameTimeUS;
                        capture.pixels.assign(s.pixels,s.pixels+s.bytes);
                        captureWaiting=true;
                    }
                });
            }
            // Retain the latest snapshot if the sender is still opening/closing
			// its codec. No retimestamping: it can later serve an explicit IDR,
			// but a frame older than two 60 Hz periods cannot masquerade as fresh.
            if(captureWaiting){
                if(!streaming||capture.key!=key||now<capture.acquiredUS||bool(capture.gpu)!=(gpuInput_.load()&&!sniper)){
                    captureWaiting=false;capture.gpu.reset();
                }
                else if(pictures_.publish(capture)){captureWaiting=false;SetEvent(pictureReady_.get());}
            }
            auto drainCopy=[&]{
                if(!copyPending)return;
                const auto completed=microseconds();
                const bool keep=streaming&&!sniper&&!gpuInput_.load()&&copyKey==key&&completed>=copyTime;
                if(readback.poll(keep?&capture.pixels:nullptr)){
                    if(keep){
                        if(!captureCount_)captureFirst_=copyTime;
                        ++captureCount_;captureLast_=copyTime;captureUS_+=completed-copyTime;captureMaxUS_=std::max(captureMaxUS_,completed-copyTime);
                        capture.key=copyKey;capture.acquiredUS=copyTime;
                        captureWaiting=!pictures_.publish(capture);
                        if(!captureWaiting)SetEvent(pictureReady_.get());
                    }
                    copyPending=false;
                }else if(completed>=copyTime&&completed-copyTime>1000000){
                    throw Failure(HRESULT_FROM_WIN32(ERROR_TIMEOUT),"Desktop GPU copy stalled for one second");
                }
            };
            drainCopy();
            IDARG_OUT_RELEASEANDACQUIREBUFFER acquired{};
            // Return the submitted surface to DWM before waiting for readback.
            // Retain at most one next surface; never discard it because the
            // staging slot is busy, and never overwrite an in-flight GPU copy.
            const auto acquireStart=microseconds();
            HRESULT result=desktop?E_PENDING:IddCxSwapChainReleaseAndAcquireBuffer(chain_,&acquired);
            if(result!=E_PENDING){
                check(result,"Acquire desktop frame");
                ComPtr<IDXGIResource> resource;resource.Attach(acquired.MetaData.pSurface);
                check(resource.As(&desktop),"Desktop texture");
                desktopKey=key;desktopTime=microseconds();
                if(streaming&&!sniper){
                    ++acquires;acquireMaxUS=std::max(acquireMaxUS,desktopTime-acquireStart);
                    const auto number=acquired.MetaData.PresentationFrameNumber;
                    if(haveFrameNumber&&number==lastFrameNumber)++duplicates;
                    else{
                        ++unique;
                        const UINT gap=number-lastFrameNumber;
                        if(haveFrameNumber&&gap>1&&gap<1000)missingIds+=gap-1;
                    }
                    haveFrameNumber=true;lastFrameNumber=number;
                    const uint64_t desired=acquired.MetaData.PresentDisplayQPCTime;
                    if(desired){
                        if(!desiredFirst)desiredFirst=desired;desiredLast=desired;
                        LARGE_INTEGER clock{};QueryPerformanceCounter(&clock);
                        if(uint64_t(clock.QuadPart)>=desired)lateMaxUS=std::max(lateMaxUS,(uint64_t(clock.QuadPart)-desired)*1000000/uint64_t(qpcFrequency.QuadPart));
                    }
                }
                D3D11_TEXTURE2D_DESC description{};desktop->GetDesc(&description);
                if(description.Width!=initial.width||description.Height!=initial.height||description.ArraySize!=1||description.MipLevels!=1||description.SampleDesc.Count!=1||
                   (description.Format!=DXGI_FORMAT_B8G8R8A8_UNORM&&description.Format!=DXGI_FORMAT_B8G8R8A8_UNORM_SRGB))
                    throw Failure(E_NOTIMPL,"Desktop differs from advertised S7 monitor mode");
            }
            bool gpuStarved=false;
            if(desktop&&!copyPending){
                if(streaming&&!sniper&&desktopKey==key){
                    if(gpu&&gpuInput_.load()){
                        capture.gpu.reset();capture.pixels.clear();
                        capture.gpu=gpu->convert(desktop.Get());
                        if(!capture.gpu){
                            gpuStarved=true;++gpuPoolWaits_;
                            if(microseconds()-desktopTime>1000000)throw Failure(HRESULT_FROM_WIN32(ERROR_TIMEOUT),"GPU samples were not returned for one second");
                        }else{
                            ++gpuFrames_;capture.key=desktopKey;capture.acquiredUS=desktopTime;
                            captureWaiting=!pictures_.publish(capture);
                            if(!captureWaiting)SetEvent(pictureReady_.get());
                        }
                    }else{
                        capture.gpu.reset();readback.submit(desktop.Get());
                        copyPending=true;copyKey=desktopKey;copyTime=desktopTime;
                    }
                }
                if(!gpuStarved){
                    desktop.Reset();check(IddCxSwapChainFinishedProcessingFrame(chain_),"Finish desktop GPU submission");
                // ReleaseAndAcquire also releases when it returns E_PENDING.
                // Do that immediately, not after a timer or staging Map succeeds.
                    continue;
                }
            }
            drainCopy();
            if(desktop&&!copyPending&&!gpuStarved)continue;
            HANDLE waits[]={stop_.get(),desktop?readbackTimer.get():available_,readbackTimer.get()};
            const bool pending=copyPending||captureWaiting||sniper||gpuStarved;
            if(pending){
                LARGE_INTEGER due{};due.QuadPart=sniper?-LONGLONG(sniperWait(primaryPoll,microseconds(),copyPending)*10):(copyPending?-10000:-20000);
                wincheck(SetWaitableTimer(readbackTimer.get(),&due,0,nullptr,nullptr,FALSE),"Wait for pending GPU copy");
            }
            DWORD wait=WaitForMultipleObjects(desktop?2:(pending?3:2),waits,FALSE,pending?INFINITE:50);
            if(wait==WAIT_OBJECT_0)break;if(wait==WAIT_FAILED)throw Failure(HRESULT_FROM_WIN32(GetLastError()),"Wait for desktop frame");
        }
    }
    void run()noexcept{
        std::thread sender;
        try{core(sender);}
        catch(const Failure& e){if(!signaled(stop_.get())){session_->failure=e.code;monitorEvent(e.what(),e.code);}}
        catch(const std::exception& e){if(!signaled(stop_.get())){session_->failure=E_FAIL;monitorEvent(e.what(),E_FAIL);}}
        catch(...){if(!signaled(stop_.get())){session_->failure=E_FAIL;monitorEvent("Unknown swap-chain failure",E_FAIL);}}
        SetEvent(stop_.get());pictures_.close();
        if(sender.joinable())sender.join(); // No detach; MFT and USB owners outlive all their work.
        const auto counts=pictures_.stats();
        char detail[512];sprintf_s(detail,"Desktop handoff: published=%llu taken=%llu replaced=%llu stale=%llu snapshots=%llu rejected=%llu readbacks=%llu span_us=%llu readback_mean_us=%llu readback_max_us=%llu",
            (unsigned long long)counts.published,(unsigned long long)counts.taken,(unsigned long long)counts.replaced,
            (unsigned long long)counts.stale,(unsigned long long)counts.refreshed,(unsigned long long)counts.rejected,
            (unsigned long long)captureCount_,(unsigned long long)(captureLast_-captureFirst_),
            (unsigned long long)(captureCount_?captureUS_/captureCount_:0),(unsigned long long)captureMaxUS_);monitorEvent(detail);
        WdfObjectDelete(chain_);chain_=nullptr;
    }
public:
    Worker(IDDCX_SWAPCHAIN chain,LUID adapter,HANDLE available,std::shared_ptr<Session> session):chain_(chain),adapter_(adapter),available_(available),session_(std::move(session)),thread_([this]{run();}){}
    ~Worker(){SetEvent(stop_.get());if(thread_.joinable())thread_.join();}
    Worker(const Worker&)=delete;Worker& operator=(const Worker&)=delete;
};
struct Monitor {
    std::shared_ptr<Session> session;const uint32_t fps,width,height;
    MonitorEdid edid;
    std::mutex mutex;std::unique_ptr<Worker> worker;
    Monitor(std::shared_ptr<Session> s,const Config& c):session(std::move(s)),fps(c.fps),width(c.width),height(c.height),edid(monitorEdid(fps,monitorSerial(TargetSerial),width,height)){}
    void stop(){std::unique_ptr<Worker> old;{std::lock_guard<std::mutex> lock(mutex);old=std::move(worker);}old.reset();}
    ~Monitor(){stop();}
    void assign(IDDCX_SWAPCHAIN chain,LUID adapter,HANDLE available){
        stop();session->failure=S_OK;
        auto next=std::make_unique<Worker>(chain,adapter,available,session);
        std::lock_guard<std::mutex> lock(mutex);worker=std::move(next);
    }
};
struct Device;
struct DeviceContext {Device* device;};struct AdapterContext {Device* device;};struct MonitorContext {Monitor* monitor;};
WDF_DECLARE_CONTEXT_TYPE(DeviceContext);
WDF_DECLARE_CONTEXT_TYPE(AdapterContext);
WDF_DECLARE_CONTEXT_TYPE(MonitorContext);
struct Device {
    MediaFoundation media; // Last to die, after watcher and all codec workers.
    WDFDEVICE device;IDDCX_ADAPTER adapter=nullptr;MonitorSlot<IDDCX_MONITOR> monitor;
    std::mutex lifecycle;Handle stop_=event();std::thread watcher;
    bool powered=false,initializing=false,ready=false;
    Device(WDFDEVICE d):device(d){}
    ~Device(){stop();}
    bool depart(){
        if(!monitor.occupied())return true;
        auto m=monitor.beginDeparture();WdfObjectGet_MonitorContext(m)->monitor->stop();
        // Only a successful departure destroys the object. On failure retain
        // the exact handle, block arrival and retry; never create a duplicate.
        NTSTATUS hr=IddCxMonitorDeparture(m);
        if(!NT_SUCCESS(hr)){log("Monitor departure failed; retaining owner",HRESULT_FROM_NT(hr));return false;}
        return monitor.finishDeparture(m,true);
    }
    void arrive(std::shared_ptr<Session> session,const Config& c){
        if(monitor.occupied())throw Failure(HRESULT_FROM_WIN32(ERROR_BUSY),"Previous S7 monitor departure is unconfirmed");
        IDDCX_MONITOR_INFO info{};info.Size=sizeof(info);info.MonitorType=DISPLAYCONFIG_OUTPUT_TECHNOLOGY_INDIRECT_WIRED;info.ConnectorIndex=0;
        info.MonitorDescription.Size=sizeof(info.MonitorDescription);info.MonitorDescription.Type=IDDCX_MONITOR_DESCRIPTION_TYPE_EDID;
        // Match the physical S7's HID container; never attach touch to the primary display.
        info.MonitorContainerId=session->snapshot().usb->containerId();
        auto object=std::make_unique<Monitor>(std::move(session),c);
        info.MonitorDescription.DataSize=static_cast<UINT>(object->edid.size());
        info.MonitorDescription.pData=object->edid.data();
        WDF_OBJECT_ATTRIBUTES attr;WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(&attr,MonitorContext);
        attr.EvtCleanupCallback=[](WDFOBJECT o){auto p=WdfObjectGet_MonitorContext(o);delete p->monitor;p->monitor=nullptr;};
        IDARG_IN_MONITORCREATE in{};in.ObjectAttributes=&attr;in.pMonitorInfo=&info;IDARG_OUT_MONITORCREATE out{};
        ntcheck(IddCxMonitorCreate(adapter,&in,&out),"Create S7 virtual monitor");WdfObjectGet_MonitorContext(out.MonitorObject)->monitor=object.release();
        IDARG_OUT_MONITORARRIVAL arrival{};NTSTATUS hr=IddCxMonitorArrival(out.MonitorObject,&arrival);
        if(!NT_SUCCESS(hr)){WdfObjectDelete(out.MonitorObject);ntcheck(hr,"Arrive S7 virtual monitor");}
        monitor.adopt(out.MonitorObject,c.fps);
    }
    void watch()noexcept{
        std::shared_ptr<Usb> usb;std::shared_ptr<Session> session;std::wstring lastPath;
        PresencePolicy presence;HRESULT lastError=S_OK;uint64_t lastLog=0;
        auto report=[&](const char* text,HRESULT code){
            auto now=microseconds();
            if(code!=lastError||now-lastLog>=5000000){log(text,code);lastError=code;lastLog=now;}
        };
        try{
            ComRuntime com;
            while(!signaled(stop_.get())){
                if(monitor.pending()){
                    if(session)session->suspend();usb.reset();
                    if(!depart()){WaitForSingleObject(stop_.get(),250);continue;}
                    session.reset();
                }
                // A successful PnP absence is independent of the WinUSB stream.
                // Never interpret camera failure, no consumer, or timeout as unplug.
                bool absent=false;
                if(!lastPath.empty()){
                    try{absent=presence.observe(Usb::present(lastPath)?Presence::Present:Presence::Absent);}
                    catch(const Failure& e){presence.observe(Presence::Unknown);report(e.what(),e.code);}
                }
                if(absent){
                    if(session)session->suspend();usb.reset();
                    if(!depart()){WaitForSingleObject(stop_.get(),250);continue;}
                    session.reset();lastPath.clear();
                }
                try{
                    if(session&&FAILED(session->transportFailure.load())){session->suspend();usb.reset();}
                    if(!usb){usb=Usb::discover();if(usb)lastPath=usb->path();}
                    if(usb){
                        Config c=usb->config(stop_.get());
                        if(!c.enabled){
                            // Explicit phone menu action, not loss of the Surface consumer.
                            if(!depart())throw Failure(E_FAIL,"S7 monitor departure not confirmed");
                            session.reset();usb->status(c,0,S_OK,stop_.get());
                        }else{
                            if(monitor.occupied()&&(monitor.fps()!=c.fps||WdfObjectGet_MonitorContext(monitor.get())->monitor->width!=c.width||WdfObjectGet_MonitorContext(monitor.get())->monitor->height!=c.height)){
                                if(!depart())throw Failure(E_FAIL,"S7 mode change waits for departure");
                                session.reset();
                            } // explicit mode change
                            if(session)session->update(usb,c);
                            // Enabled owns Windows visibility. A local fullscreen
                            // camera can pause the consumer without unplugging IDD.
                            if(!monitor.occupied()){session=std::make_shared<Session>(usb,c);arrive(session,c);}
                            if(!c.consumer)usb->status(c,0,S_OK,stop_.get());
                            else if(session&&FAILED(session->failure.load()))usb->status(c,3,session->failure.load(),stop_.get());
                        }
                    }
                }catch(const Failure& e){
                    if(!signaled(stop_.get()))report(e.what(),e.code);
                    if(session)session->suspend();usb.reset();
                }catch(const std::exception& e){
                    report(e.what(),E_FAIL);if(session)session->suspend();usb.reset();
                }
                WaitForSingleObject(stop_.get(),250);
            }
        }catch(const std::exception& e){log(e.what(),E_FAIL);}
        if(session)session->suspend();
        (void)depart();session.reset();usb.reset();
    }
    void launchLocked(){if(powered&&ready&&!watcher.joinable()){ResetEvent(stop_.get());watcher=std::thread([this]{watch();});}}
    void finish(NTSTATUS status){std::lock_guard<std::mutex> lock(lifecycle);initializing=false;ready=NT_SUCCESS(status);if(ready)launchLocked();else log("IddCx adapter initialization failed",HRESULT_FROM_NT(status));}
    NTSTATUS start(){
        std::lock_guard<std::mutex> lock(lifecycle);powered=true;if(ready){launchLocked();return STATUS_SUCCESS;}if(initializing)return STATUS_SUCCESS;
        IDDCX_ENDPOINT_VERSION version{};version.Size=sizeof(version);version.MajorVer=3;
        IDDCX_ADAPTER_CAPS caps{};caps.Size=sizeof(caps);caps.MaxMonitorsSupported=1;
        auto& d=caps.EndPointDiagnostics;d.Size=sizeof(d);d.GammaSupport=IDDCX_FEATURE_IMPLEMENTATION_NONE;d.TransmissionType=IDDCX_TRANSMISSION_TYPE_WIRED_OTHER;
        d.pEndPointFriendlyName=L"S7 H.264 Monitor";d.pEndPointManufacturerName=L"S7 Appliance Project";d.pEndPointModelName=L"SM-G930F / S7M1 v2";d.pFirmwareVersion=&version;d.pHardwareVersion=&version;
        WDF_OBJECT_ATTRIBUTES attr;WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(&attr,AdapterContext);
        IDARG_IN_ADAPTER_INIT in{};in.WdfDevice=device;in.pCaps=&caps;in.ObjectAttributes=&attr;IDARG_OUT_ADAPTER_INIT out{};
        initializing=true;NTSTATUS status=IddCxAdapterInitAsync(&in,&out);
        if(NT_SUCCESS(status)){adapter=out.AdapterObject;WdfObjectGet_AdapterContext(adapter)->device=this;}else initializing=false;return status;
    }
    void stop(){std::thread old;{std::lock_guard<std::mutex> lock(lifecycle);powered=false;SetEvent(stop_.get());old=std::move(watcher);}if(old.joinable())old.join();}
};
static void fillSignal(DISPLAYCONFIG_VIDEO_SIGNAL_INFO& s,uint32_t fps,bool monitor,uint32_t width=1280,uint32_t height=720){
    MonitorTiming t;
    if(!monitorTiming(fps,t,width,height))throw Failure(E_INVALIDARG,"Unsupported S7 monitor timing");
    s.activeSize.cx=t.width;s.activeSize.cy=t.height;
    s.totalSize.cx=t.totalWidth;s.totalSize.cy=t.totalHeight;
    s.AdditionalSignalInfo.videoStandard=255;s.AdditionalSignalInfo.vSyncFreqDivider=monitor?0:1;
    s.vSyncFreq={t.fps,1};s.hSyncFreq={t.pixelClock,t.totalWidth};s.pixelRate=t.pixelClock;
    s.scanLineOrdering=DISPLAYCONFIG_SCANLINE_ORDERING_PROGRESSIVE;
}
}
EVT_WDF_DRIVER_DEVICE_ADD S7DeviceAdd;
EVT_WDF_DEVICE_D0_ENTRY S7D0Entry;EVT_WDF_DEVICE_D0_EXIT S7D0Exit;
EVT_IDD_CX_ADAPTER_INIT_FINISHED S7AdapterFinished;
EVT_IDD_CX_ADAPTER_COMMIT_MODES S7CommitModes;
EVT_IDD_CX_PARSE_MONITOR_DESCRIPTION S7ParseDescription;
EVT_IDD_CX_MONITOR_GET_DEFAULT_DESCRIPTION_MODES S7DefaultModes;
EVT_IDD_CX_MONITOR_QUERY_TARGET_MODES S7TargetModes;
EVT_IDD_CX_MONITOR_ASSIGN_SWAPCHAIN S7Assign;
EVT_IDD_CX_MONITOR_UNASSIGN_SWAPCHAIN S7Unassign;
extern "C" DRIVER_INITIALIZE DriverEntry;
extern "C" BOOL WINAPI DllMain(HINSTANCE,DWORD,LPVOID){return TRUE;}
extern "C" NTSTATUS DriverEntry(PDRIVER_OBJECT object,PUNICODE_STRING registry){
    WDF_DRIVER_CONFIG config;WDF_DRIVER_CONFIG_INIT(&config,S7DeviceAdd);WDF_OBJECT_ATTRIBUTES attr;WDF_OBJECT_ATTRIBUTES_INIT(&attr);
    return WdfDriverCreate(object,registry,&attr,&config,WDF_NO_HANDLE);
}
NTSTATUS S7DeviceAdd(WDFDRIVER,PWDFDEVICE_INIT init){
    using namespace s7;
    WDF_PNPPOWER_EVENT_CALLBACKS power;WDF_PNPPOWER_EVENT_CALLBACKS_INIT(&power);power.EvtDeviceD0Entry=S7D0Entry;power.EvtDeviceD0Exit=S7D0Exit;WdfDeviceInitSetPnpPowerEventCallbacks(init,&power);
    IDD_CX_CLIENT_CONFIG client;IDD_CX_CLIENT_CONFIG_INIT(&client);
    client.EvtIddCxAdapterInitFinished=S7AdapterFinished;client.EvtIddCxAdapterCommitModes=S7CommitModes;client.EvtIddCxParseMonitorDescription=S7ParseDescription;
    client.EvtIddCxMonitorGetDefaultDescriptionModes=S7DefaultModes;client.EvtIddCxMonitorQueryTargetModes=S7TargetModes;client.EvtIddCxMonitorAssignSwapChain=S7Assign;client.EvtIddCxMonitorUnassignSwapChain=S7Unassign;
    NTSTATUS status=IddCxDeviceInitConfig(init,&client);if(!NT_SUCCESS(status))return status;
    WDF_OBJECT_ATTRIBUTES attr;WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE(&attr,DeviceContext);
    attr.EvtCleanupCallback=[](WDFOBJECT object){auto p=s7::WdfObjectGet_DeviceContext(object);delete p->device;p->device=nullptr;};
    WDFDEVICE device=nullptr;status=WdfDeviceCreate(&init,&attr,&device);if(!NT_SUCCESS(status))return status;
    status=IddCxDeviceInitialize(device);if(!NT_SUCCESS(status))return status;
    try{s7::WdfObjectGet_DeviceContext(device)->device=new s7::Device(device);}catch(...){return STATUS_INSUFFICIENT_RESOURCES;}return STATUS_SUCCESS;
}
NTSTATUS S7D0Entry(WDFDEVICE d,WDF_POWER_DEVICE_STATE){try{return s7::WdfObjectGet_DeviceContext(d)->device->start();}catch(...){return STATUS_INSUFFICIENT_RESOURCES;}}
NTSTATUS S7D0Exit(WDFDEVICE d,WDF_POWER_DEVICE_STATE){s7::monitorEvent("S7 monitor device D0 exit");s7::WdfObjectGet_DeviceContext(d)->device->stop();return STATUS_SUCCESS;}
NTSTATUS S7AdapterFinished(IDDCX_ADAPTER adapter,const IDARG_IN_ADAPTER_INIT_FINISHED* in){try{s7::WdfObjectGet_AdapterContext(adapter)->device->finish(in->AdapterInitStatus);return STATUS_SUCCESS;}catch(...){return STATUS_INSUFFICIENT_RESOURCES;}}
NTSTATUS S7CommitModes(IDDCX_ADAPTER,const IDARG_IN_COMMITMODES*){return STATUS_SUCCESS;}
NTSTATUS S7ParseDescription(const IDARG_IN_PARSEMONITORDESCRIPTION* in,IDARG_OUT_PARSEMONITORDESCRIPTION* out){
    if(!in || !out)return STATUS_INVALID_PARAMETER;
    s7::MonitorTiming t;
    if(in->MonitorDescription.Type!=IDDCX_MONITOR_DESCRIPTION_TYPE_EDID ||
       !s7::parseMonitorEdid(in->MonitorDescription.pData,in->MonitorDescription.DataSize,t))return STATUS_INVALID_PARAMETER;
    out->MonitorModeBufferOutputCount=1;out->PreferredMonitorModeIdx=0;
    if(!in->MonitorModeBufferInputCount || !in->pMonitorModes)return STATUS_SUCCESS;
    IDDCX_MONITOR_MODE m{};m.Size=sizeof(m);m.Origin=IDDCX_MONITOR_MODE_ORIGIN_MONITORDESCRIPTOR;
    s7::fillSignal(m.MonitorVideoSignalInfo,t.fps,true,t.width,t.height);
    in->pMonitorModes[0]=m;return STATUS_SUCCESS;
}
NTSTATUS S7DefaultModes(IDDCX_MONITOR monitor,const IDARG_IN_GETDEFAULTDESCRIPTIONMODES* in,IDARG_OUT_GETDEFAULTDESCRIPTIONMODES* out){
    out->DefaultMonitorModeBufferOutputCount=1;out->PreferredMonitorModeIdx=0;if(!in->DefaultMonitorModeBufferInputCount)return STATUS_SUCCESS;
    if(!in->pDefaultMonitorModes)return STATUS_INVALID_PARAMETER;
    IDDCX_MONITOR_MODE m{};m.Size=sizeof(m);m.Origin=IDDCX_MONITOR_MODE_ORIGIN_DRIVER;
    const auto target=s7::WdfObjectGet_MonitorContext(monitor)->monitor;
    s7::fillSignal(m.MonitorVideoSignalInfo,target->fps,true,target->width,target->height);in->pDefaultMonitorModes[0]=m;return STATUS_SUCCESS;
}
NTSTATUS S7TargetModes(IDDCX_MONITOR monitor,const IDARG_IN_QUERYTARGETMODES* in,IDARG_OUT_QUERYTARGETMODES* out){
    out->TargetModeBufferOutputCount=1;if(!in->TargetModeBufferInputCount)return STATUS_SUCCESS;if(!in->pTargetModes)return STATUS_INVALID_PARAMETER;
    const auto target=s7::WdfObjectGet_MonitorContext(monitor)->monitor;
    IDDCX_TARGET_MODE mode{};mode.Size=sizeof(mode);s7::fillSignal(mode.TargetVideoSignalInfo.targetVideoSignalInfo,target->fps,false,target->width,target->height);in->pTargetModes[0]=mode;return STATUS_SUCCESS;
}
NTSTATUS S7Assign(IDDCX_MONITOR monitor,const IDARG_IN_SETSWAPCHAIN* in){
    try{s7::WdfObjectGet_MonitorContext(monitor)->monitor->assign(in->hSwapChain,in->RenderAdapterLuid,in->hNextSurfaceAvailable);return STATUS_SUCCESS;}
    catch(const s7::Failure& e){auto m=s7::WdfObjectGet_MonitorContext(monitor)->monitor;m->session->failure=e.code;s7::log(e.what(),e.code);}
    catch(...){s7::WdfObjectGet_MonitorContext(monitor)->monitor->session->failure=E_OUTOFMEMORY;}
    WdfObjectDelete(in->hSwapChain);return STATUS_SUCCESS;
}
NTSTATUS S7Unassign(IDDCX_MONITOR monitor){s7::monitorEvent("S7 monitor swapchain unassigned");s7::WdfObjectGet_MonitorContext(monitor)->monitor->stop();return STATUS_SUCCESS;}
