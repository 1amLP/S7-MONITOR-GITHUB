#include "Sniper.h"
#include "../monitor/SniperChannel.h"
#include "../monitor/SniperGeometry.h"
#include "../monitor/SniperCadence.h"
#include "../monitor/SniperTouch.h"
#include "../monitor/SniperHidProtocol.h"
#include "SniperMapping.h"
#include "Preference.h"
#include "../monitor/SniperRecovery.h"
#include "../monitor/MonitorTrace.h"
#include "../monitor/DesktopReadback.h"
#include <dxgi1_2.h>
#include <d3dcompiler.h>
#include <userenv.h>
#include <cstdio>
#include <atomic>
#include <thread>

namespace {
using namespace s7;
std::atomic<bool> desktopStop{false};
BOOL WINAPI stopDesktop(DWORD){desktopStop=true;return TRUE;}
struct Crop {
    RECT source{},target{};
    SniperTransform transform;
    Crop(const SniperRequest& r,LONG w,LONG h){
        const auto c=sniperCrop(w,h,r.zoom,r.scaleX,r.scaleY,r.x,r.y,r.stretch!=0);
        if(c.width<=0||c.height<=0)throw Failure(E_INVALIDARG,"Sniper crop geometry");
        source={c.left,c.top,c.left+c.width,c.top+c.height};
        target={0,0,LONG(r.width),LONG(r.height)};
        transform=sniperTransform(c,w,h,r.rotation,r.mirror!=0);
    }
};
class Pointer {
    std::unique_ptr<s7camera::Preference> device_;
    SniperHidMessage last_;
    HANDLE stop_;
    uint64_t lastInjection_=0;
    bool inject(const SniperHidMessage& message){
        try{
            if(!device_)device_=std::make_unique<s7camera::Preference>(USHORT(4),USHORT(SniperHidBytes));
            const auto b=message.encode();
            s7camera::transferFeature(device_->handle(),Bytes(b.begin(),b.end()),true,stop_);
            Bytes query(SniperHidBytes);query[0]=SniperFeatureID;
            const auto ack=SniperHidMessage::parse(s7camera::transferFeature(device_->handle(),std::move(query),false,stop_));
            if(ack.kind!=HidStatus||ack.generation!=message.generation||ack.sequence!=message.sequence||ack.status==HidPending)return false;
            if(ack.status!=HidOK)throw Failure(HRESULT_FROM_WIN32(ack.status==HidStale?ERROR_INVALID_STATE:ERROR_DEVICE_NOT_AVAILABLE),"Sniper hardware HID refused input");
        }catch(HRESULT hr){device_.reset();throw Failure(hr,"Sniper HID feature transfer");}
        const bool wasDown=last_.down;last_=message;lastInjection_=microseconds();
        if(wasDown!=last_.down)monitorEvent(last_.down?"Sniper HID DOWN written":"Sniper HID UP written");
        return true;
    }
public:
    explicit Pointer(HANDLE stop):stop_(stop){}
    ~Pointer(){release();}
    void canceledByWindows()noexcept{release();}
    void release()noexcept{
        if(!last_.generation)return;
        try{SniperHidMessage up;up.kind=HidCancel;up.generation=last_.generation;(void)inject(up);}
        catch(const Failure& e){monitorEvent(e.what(),e.code);}
        last_={};
    }
    void maintain(){
        if(last_.down&&microseconds()-lastInjection_>=100000)(void)inject(last_);
    }
    bool apply(const SniperRequest& r,RECT desktop){
        SniperHidMessage m;m.kind=HidPoint;m.generation=r.generation;m.sequence=static_cast<uint16_t>(r.sequence);
        if(r.down){
            Crop crop(r,desktop.right-desktop.left,desktop.bottom-desktop.top);
            float sx=0,sy=0;
            if(crop.transform.map(float(r.contactX)/32767,float(r.contactY)/32767,sx,sy)){
                m.down=true;m.x=uint16_t(std::lround(sx*32767));m.y=uint16_t(std::lround(sy*32767));
            }
        }
        return inject(m);
    }
};

void sniperInputLoop(DWORD session,HANDLE stop)noexcept{
    try{
        SetThreadDpiAwarenessContext(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2);
        SniperChannel channel(session);Pointer pointer(stop);uint32_t generation=0;uint16_t sequence=0;
        bool blocked=false;HRESULT failure=S_OK;uint32_t notified=0;
        while(WaitForSingleObject(stop,5)==WAIT_TIMEOUT){
            SniperRequest r;RECT desktop{};uint32_t geometryGeneration=0,applied=0;HRESULT mapping=E_PENDING;
            const bool copied=channel.access([&](SniperShared& s){r=s.request;desktop=s.primaryDesktop;geometryGeneration=s.primaryGeneration;applied=s.mappingApplied;mapping=s.mappingError;});
            if(!copied||!r.fresh()||geometryGeneration!=r.generation){pointer.release();continue;}
            try{
                const auto device=sniperPointerDevice();
                if(!device.device){pointer.release();continue;}
                channel.access([&](SniperShared& s){if(std::wstring(s.touchDevice)!=device.path){wcscpy_s(s.touchDevice,device.path.c_str());if(++s.mappingRequested==0)++s.mappingRequested;s.mappingError=E_PENDING;}});
                if(applied&&applied!=notified&&mapping==S_OK){
                    DWORD_PTR ignored=0;SendMessageTimeoutW(HWND_BROADCAST,WM_SETTINGCHANGE,0,reinterpret_cast<LPARAM>(L"TabletPCDigitizerMappingChanged"),SMTO_ABORTIFHUNG,200,&ignored);notified=applied;
                }
                if(device.display.left!=desktop.left||device.display.top!=desktop.top||device.display.right!=desktop.right||device.display.bottom!=desktop.bottom){pointer.release();continue;}
                if(generation!=r.generation){const bool first=generation==0;pointer.release();generation=r.generation;sequence=0;blocked=false;if(first){sequence=static_cast<uint16_t>(r.sequence);blocked=r.down!=0;}}
                if(r.sequence&&sequence!=r.sequence){
                    if(blocked&&r.down)sequence=static_cast<uint16_t>(r.sequence);
                    else if(pointer.apply(r,desktop)){sequence=static_cast<uint16_t>(r.sequence);blocked=false;failure=S_OK;}
                }else if(!blocked)pointer.maintain();
            }catch(const Failure& e){
                pointer.release();blocked=true;sequence=static_cast<uint16_t>(r.sequence);
                if(failure!=e.code){monitorEvent("Sniper HID input failed; video continues",e.code);failure=e.code;}
            }
            channel.access([&](SniperShared& s){if(s.request.generation==generation)s.acceptedSequence=sequence;});
        }
    }catch(const Failure& e){monitorEvent(e.what(),e.code);}
    catch(...){monitorEvent("Sniper HID worker failed",E_FAIL);}
}
constexpr char shader[]=R"(
Texture2D image : register(t0); SamplerState linearClamp : register(s0);
cbuffer Geometry : register(b0) {float4 rowX; float4 rowY; uint rotation; float3 pad;};
struct Vertex {float4 p:SV_Position; float2 uv:TEXCOORD0;};
Vertex vs(uint id:SV_VertexID){Vertex v;v.uv=float2((id<<1)&2,id&2);v.p=float4(v.uv*float2(2,-2)+float2(-1,1),0,1);return v;}
float4 ps(Vertex v):SV_Target{
 float3 uv=float3(v.uv,1);
 float2 q=float2(dot(rowX.xyz,uv),dot(rowY.xyz,uv));
 if(any(q<0)||any(q>1))return float4(0,0,0,1);
 if(rotation==2)q=float2(q.y,1-q.x);
 else if(rotation==3)q=1-q;
 else if(rotation==4)q=float2(1-q.y,q.x);
 return float4(image.SampleLevel(linearClamp,q,0).rgb,1);
})";
class PrimaryCapture {
    ComPtr<ID3D11Device> device_;ComPtr<ID3D11DeviceContext> context_;
    ComPtr<IDXGIOutputDuplication> duplication_;
    ComPtr<ID3D11Texture2D> source_,target_;
    ComPtr<ID3D11ShaderResourceView> sourceView_;
    ComPtr<ID3D11RenderTargetView> targetView_;
    ComPtr<ID3D11VertexShader> vertex_;ComPtr<ID3D11PixelShader> pixel_;
    ComPtr<ID3D11SamplerState> sampler_;ComPtr<ID3D11Buffer> geometry_;
    std::unique_ptr<DesktopReadback> readback_;
    DXGI_OUTPUT_DESC output_{};
    unsigned width_=0,height_=0;
    bool pending_=false,haveSource_=false;
    uint64_t frameTime_=0;
    SniperRequest drawn_{};
public:
    RECT desktop()const{return output_.DesktopCoordinates;}
    std::wstring displayDevice()const{DISPLAY_DEVICEW d{sizeof(d)};wincheck(EnumDisplayDevicesW(output_.DeviceName,0,&d,EDD_GET_DEVICE_INTERFACE_NAME),"Primary monitor identity");return d.DeviceID;}
    bool pending()const{return pending_;}
    PrimaryCapture(unsigned width,unsigned height):width_(width),height_(height){
        if(!Config::mode(width,height))throw Failure(E_INVALIDARG,"Sniper output mode");
        ComPtr<IDXGIFactory1> factory;check(CreateDXGIFactory1(IID_PPV_ARGS(&factory)),"Sniper DXGI factory");
        ComPtr<IDXGIAdapter1> selected;ComPtr<IDXGIOutput1> output;
        const HMONITOR primary=MonitorFromPoint(POINT{0,0},MONITOR_DEFAULTTOPRIMARY);
        for(UINT a=0;!output;++a){
            ComPtr<IDXGIAdapter1> adapter;HRESULT hr=factory->EnumAdapters1(a,&adapter);if(hr==DXGI_ERROR_NOT_FOUND)break;check(hr,"Sniper GPU enumeration");
            for(UINT o=0;;++o){
                ComPtr<IDXGIOutput> candidate;hr=adapter->EnumOutputs(o,&candidate);if(hr==DXGI_ERROR_NOT_FOUND)break;check(hr,"Sniper output enumeration");
                DXGI_OUTPUT_DESC desc{};check(candidate->GetDesc(&desc),"Sniper output description");
                if(desc.AttachedToDesktop&&desc.Monitor==primary){selected=adapter;check(candidate.As(&output),"Sniper duplication output");output_=desc;break;}
            }
        }
        if(!output)throw Failure(DXGI_ERROR_NOT_FOUND,"Primary desktop is not available");
        check(D3D11CreateDevice(selected.Get(),D3D_DRIVER_TYPE_UNKNOWN,nullptr,D3D11_CREATE_DEVICE_BGRA_SUPPORT,nullptr,0,D3D11_SDK_VERSION,&device_,nullptr,&context_),"Sniper GPU device");
        check(output->DuplicateOutput(device_.Get(),&duplication_),"Primary desktop duplication");
        DXGI_OUTDUPL_DESC capture{};duplication_->GetDesc(&capture);
        D3D11_TEXTURE2D_DESC desc{};desc.Width=capture.ModeDesc.Width;desc.Height=capture.ModeDesc.Height;desc.Format=DXGI_FORMAT_B8G8R8A8_UNORM;
        desc.ArraySize=desc.MipLevels=desc.SampleDesc.Count=1;desc.Usage=D3D11_USAGE_DEFAULT;desc.BindFlags=D3D11_BIND_SHADER_RESOURCE;
        check(device_->CreateTexture2D(&desc,nullptr,&source_),"Sniper owned desktop texture");
        check(device_->CreateShaderResourceView(source_.Get(),nullptr,&sourceView_),"Sniper desktop SRV");
        desc.Width=width;desc.Height=height;desc.BindFlags=D3D11_BIND_RENDER_TARGET;
        check(device_->CreateTexture2D(&desc,nullptr,&target_),"Sniper scaled target");
        check(device_->CreateRenderTargetView(target_.Get(),nullptr,&targetView_),"Sniper render target");
        struct Library{HMODULE h;~Library(){if(h)FreeLibrary(h);}}compiler{LoadLibraryExW(L"d3dcompiler_47.dll",nullptr,LOAD_LIBRARY_SEARCH_SYSTEM32)};
        wincheck(compiler.h!=nullptr,"Sniper system shader compiler");
        auto compile=reinterpret_cast<decltype(&D3DCompile)>(GetProcAddress(compiler.h,"D3DCompile"));
        wincheck(compile!=nullptr,"Sniper shader compiler entry");
        ComPtr<ID3DBlob> vs,ps,error;
        check(compile(shader,sizeof(shader)-1,"S7.Sniper",nullptr,nullptr,"vs","vs_5_0",D3DCOMPILE_ENABLE_STRICTNESS|D3DCOMPILE_OPTIMIZATION_LEVEL3,0,&vs,&error),"Compile Sniper vertex shader");
        check(compile(shader,sizeof(shader)-1,"S7.Sniper",nullptr,nullptr,"ps","ps_5_0",D3DCOMPILE_ENABLE_STRICTNESS|D3DCOMPILE_OPTIMIZATION_LEVEL3,0,&ps,&error),"Compile Sniper pixel shader");
        check(device_->CreateVertexShader(vs->GetBufferPointer(),vs->GetBufferSize(),nullptr,&vertex_),"Sniper vertex shader");
        check(device_->CreatePixelShader(ps->GetBufferPointer(),ps->GetBufferSize(),nullptr,&pixel_),"Sniper pixel shader");
        D3D11_SAMPLER_DESC sd{};sd.Filter=D3D11_FILTER_MIN_MAG_MIP_LINEAR;sd.AddressU=sd.AddressV=sd.AddressW=D3D11_TEXTURE_ADDRESS_CLAMP;sd.MaxLOD=D3D11_FLOAT32_MAX;
        check(device_->CreateSamplerState(&sd,&sampler_),"Sniper linear sampling");
        D3D11_BUFFER_DESC cb{};cb.ByteWidth=48;cb.Usage=D3D11_USAGE_DEFAULT;cb.BindFlags=D3D11_BIND_CONSTANT_BUFFER;
        check(device_->CreateBuffer(&cb,nullptr,&geometry_),"Sniper geometry constants");
        readback_=std::make_unique<DesktopReadback>(device_.Get(),context_.Get(),width,height);
    }
    bool submit(const SniperRequest& r){
        if(pending_)return false;
        DXGI_OUTDUPL_FRAME_INFO info{};ComPtr<IDXGIResource> resource;
        bool changed=!haveSource_||drawn_.generation!=r.generation||drawn_.zoom!=r.zoom||drawn_.x!=r.x||drawn_.y!=r.y||drawn_.stretch!=r.stretch||drawn_.scaleX!=r.scaleX||drawn_.scaleY!=r.scaleY||drawn_.rotation!=r.rotation||drawn_.mirror!=r.mirror;
        const HRESULT hr=duplication_->AcquireNextFrame(0,&info,&resource);
        if(hr!=DXGI_ERROR_WAIT_TIMEOUT){
            check(hr,"Acquire primary desktop");
            struct Release{IDXGIOutputDuplication* d;~Release(){d->ReleaseFrame();}}release{duplication_.Get()};
            ComPtr<ID3D11Texture2D> image;check(resource.As(&image),"Primary desktop texture");
            D3D11_TEXTURE2D_DESC a{},b{};image->GetDesc(&a);source_->GetDesc(&b);
            if(a.Width!=b.Width||a.Height!=b.Height||a.Format!=b.Format)throw Failure(DXGI_ERROR_ACCESS_LOST,"Primary desktop mode changed");
            context_->CopyResource(source_.Get(),image.Get());haveSource_=true;
            changed=changed||info.LastPresentTime.QuadPart!=0;
        }
        if(!haveSource_||!changed)return false;
        // Static desktop pixels are retained on GPU so zoom/position changes
        // redraw immediately without waiting for DWM to produce another frame.
        const LONG w=output_.DesktopCoordinates.right-output_.DesktopCoordinates.left,h=output_.DesktopCoordinates.bottom-output_.DesktopCoordinates.top;
        Crop crop(r,w,h);
        struct Constants{SniperTransform transform;UINT rotation,pad[3];} c{crop.transform,UINT(output_.Rotation),{}};
        static_assert(sizeof(Constants)==48);
        context_->UpdateSubresource(geometry_.Get(),0,nullptr,&c,0,0);
        D3D11_VIEWPORT viewport{0,0,float(width_),float(height_),0,1};context_->RSSetViewports(1,&viewport);
        auto* target=targetView_.Get();context_->OMSetRenderTargets(1,&target,nullptr);
        context_->IASetPrimitiveTopology(D3D11_PRIMITIVE_TOPOLOGY_TRIANGLELIST);context_->IASetInputLayout(nullptr);
        context_->VSSetShader(vertex_.Get(),nullptr,0);context_->PSSetShader(pixel_.Get(),nullptr,0);
        auto* source=sourceView_.Get();auto* sampler=sampler_.Get();auto* constants=geometry_.Get();
        context_->PSSetShaderResources(0,1,&source);context_->PSSetSamplers(0,1,&sampler);context_->PSSetConstantBuffers(0,1,&constants);
        context_->Draw(3,0);source=nullptr;target=nullptr;context_->PSSetShaderResources(0,1,&source);context_->OMSetRenderTargets(1,&target,nullptr);
        frameTime_=microseconds();readback_->submit(target_.Get());pending_=true;drawn_=r;
        return true;
    }
    bool poll(Bytes& pixels,uint64_t& at){if(!pending_||!readback_->poll(&pixels))return false;pending_=false;at=frameTime_;return true;}
};
}

int runSniperDesktop(){
    using namespace s7;
    SetProcessDpiAwarenessContext(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2);
    SetConsoleCtrlHandler(stopDesktop,TRUE);
    try{
        DWORD session=0;wincheck(ProcessIdToSessionId(GetCurrentProcessId(),&session),"Sniper session identity");
        SniperChannel channel(session);
        Handle inputStop(CreateEventW(nullptr,TRUE,FALSE,nullptr));wincheck(bool(inputStop),"Sniper HID stop event");
        std::thread input([&]{sniperInputLoop(session,inputStop.get());});
        struct Join{HANDLE stop;std::thread& thread;~Join(){SetEvent(stop);thread.join();}}join{inputStop.get(),input};
        std::unique_ptr<PrimaryCapture> capture;
        std::wstring displayName;
        uint32_t generation=0;uint64_t next=0;
        SniperRequest submitted{};Bytes pixels;
        Handle timer(CreateWaitableTimerExW(nullptr,nullptr,0x2,TIMER_MODIFY_STATE|SYNCHRONIZE));wincheck(bool(timer),"Sniper cadence timer");
        while(!desktopStop){
            SniperRequest r;bool copied=channel.access([&](SniperShared& s){r=s.request;});
            if(copied&&!r.fresh()){
                char detail[200]{};sprintf_s(detail,"Sniper request expired: gen=%u seq=%u age_ms=%llu valid=%u",r.generation,r.sequence,static_cast<unsigned long long>(GetTickCount64()-r.atMS),unsigned(r.valid()));
                monitorEvent(detail);break;
            }
            if(copied){
                if(generation!=r.generation){
                    capture.reset();capture=std::make_unique<PrimaryCapture>(r.width,r.height);generation=r.generation;
                    displayName=capture->displayDevice();
                }
                channel.access([&](SniperShared& s){s.primaryDesktop=capture->desktop();s.primaryGeneration=generation;if(std::wstring(s.primaryDevice)!=displayName){wcscpy_s(s.primaryDevice,displayName.c_str());if(++s.mappingRequested==0)++s.mappingRequested;s.mappingError=E_PENDING;}});
                uint64_t at=0;
                if(capture->poll(pixels,at)){
                    channel.access([&](SniperShared& s){
                        if(s.request.generation!=submitted.generation||pixels.size()>SniperMaxBytes)return;
                        std::memcpy(s.pixels,pixels.data(),pixels.size());s.bytes=uint32_t(pixels.size());s.frameGeneration=submitted.generation;s.frameTimeUS=at;++s.frameSequence;s.error=S_OK;
                    });
                }
                const auto now=microseconds();
                if(now>=next&&!capture->pending()&&capture->submit(r)){submitted=r;next=sniperDeadline(next,now);}
            }
            LARGE_INTEGER due{};due.QuadPart=-LONGLONG(sniperWait(next,microseconds(),capture&&capture->pending())*10);
            wincheck(SetWaitableTimer(timer.get(),&due,0,nullptr,nullptr,FALSE),"Sniper cadence");WaitForSingleObject(timer.get(),INFINITE);
        }
        return 0;
    }catch(const Failure& e){std::fprintf(stderr,"S7 Sniper: %s (0x%08lx)\n",e.what(),static_cast<unsigned long>(e.code));monitorEvent(e.what(),e.code);return int(e.code);}
    catch(...){return int(E_FAIL);}
}

void runSniperBroker(HANDLE stop){
    using namespace s7;
    std::unique_ptr<SniperChannel> channel;
    Handle child,job(CreateJobObjectW(nullptr,nullptr));
    if(!job)return;
    JOBOBJECT_EXTENDED_LIMIT_INFORMATION limits{};limits.BasicLimitInformation.LimitFlags=JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
    if(!SetInformationJobObject(job.get(),JobObjectExtendedLimitInformation,&limits,sizeof(limits)))return;
    DWORD session=0xffffffff;uint32_t launched=0;HRESULT lastError=S_OK;
    SniperRestart restart;
    while(WaitForSingleObject(stop,250)==WAIT_TIMEOUT){
        try{
            const DWORD current=WTSGetActiveConsoleSessionId();
            if(current==0xffffffff)continue;
            if(child&&WaitForSingleObject(child.get(),0)==WAIT_OBJECT_0){
                DWORD result=0;GetExitCodeProcess(child.get(),&result);
                const bool retry=restart.exited(result,microseconds());
                monitorEvent(retry?"Sniper worker exited; retry after fresh request":"Sniper worker exited; retry budget exhausted or permanent failure",HRESULT(result));
                if(!retry&&channel)channel->access([&](SniperShared& s){if(s.request.generation==launched)s.error=result?HRESULT(result):HRESULT_FROM_WIN32(ERROR_RETRY);});
                child.reset();
            }
            if(session!=current){
                if(channel)channel->access([](SniperShared& s){s.request.atMS=0;});
                if(child)continue;
                channel.reset();launched=0;session=current;restart={};
            }
            HANDLE raw=nullptr;wincheck(WTSQueryUserToken(session,&raw),"Sniper console user token");Handle token(raw);
            if(!channel)channel=std::make_unique<SniperChannel>(session,token.get());
            SniperRequest request;uint32_t mappingRequest=0,mappingDone=0;std::wstring touch,display;
            channel->access([&](SniperShared& s){request=s.request;mappingRequest=s.mappingRequested;mappingDone=s.mappingApplied;touch=s.touchDevice;display=s.primaryDevice;});
            if(request.fresh()&&mappingRequest!=mappingDone&&!touch.empty()&&!display.empty()){
                HRESULT result=S_OK;try{writeSniperMapping(touch,display);}catch(const Failure& e){result=e.code;monitorEvent(e.what(),e.code);}
                channel->access([&](SniperShared& s){if(s.mappingRequested==mappingRequest){s.mappingApplied=mappingRequest;s.mappingError=result;}});
            }
            if(!request.fresh()||child||!restart.ready(request.generation,microseconds()))continue;
            wchar_t path[32768]{};DWORD n=GetModuleFileNameW(nullptr,path,DWORD(std::size(path)));wincheck(n&&n<std::size(path),"Sniper executable path");
            std::wstring command=L"\""+std::wstring(path)+L"\" desktop";
            STARTUPINFOW start{sizeof(start)};start.lpDesktop=const_cast<wchar_t*>(L"winsta0\\default");
            PROCESS_INFORMATION process{};
            wincheck(CreateProcessAsUserW(token.get(),path,command.data(),nullptr,nullptr,FALSE,CREATE_NO_WINDOW|CREATE_SUSPENDED,nullptr,nullptr,&start,&process),"Start Sniper in interactive user session");
            Handle processHandle(process.hProcess),thread(process.hThread);
            if(!AssignProcessToJobObject(job.get(),processHandle.get())){TerminateProcess(processHandle.get(),ERROR_CANCELLED);throw Failure(HRESULT_FROM_WIN32(GetLastError()),"Own Sniper process lifetime");}
            wincheck(ResumeThread(thread.get())!=DWORD(-1),"Start Sniper capture thread");
            child=std::move(processHandle);launched=request.generation;lastError=S_OK;
        }catch(const Failure& e){if(e.code!=lastError){log(e.what(),e.code);lastError=e.code;}}
        catch(...){log("Sniper broker error",E_FAIL);}
    }
    if(channel)channel->access([](SniperShared& s){s.request.atMS=0;});
    if(child)WaitForSingleObject(child.get(),1500);
}
