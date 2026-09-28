// Exact-format Windows webcam FPS probe. No mode fallback, no video recording.
// Windows SDK / MSVC required. Portable arithmetic is tested separately.
#define NOMINMAX
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mfreadwrite.h>
#include <mferror.h>
#include <wrl.h>
#include <algorithm>
#include <chrono>
#include <filesystem>
#include <iomanip>
#include <iostream>
#include <mutex>
#include <sstream>
#include <string>
#include <stdexcept>
#include "FpsMetrics.h"
#include "FrameRateMatch.h"
using Microsoft::WRL::ComPtr;
using Clock=std::chrono::steady_clock;
constexpr DWORD Video=static_cast<DWORD>(MF_SOURCE_READER_FIRST_VIDEO_STREAM);
struct Problem { const char* status; HRESULT hr; };
void check(HRESULT h,const char* where="capture_error"){if(FAILED(h))throw Problem{where,h};}
double nowSeconds(){return std::chrono::duration<double>(Clock::now().time_since_epoch()).count();}
struct Packet {HRESULT status=S_OK;DWORD flags=0;LONGLONG pts=0;double arrival=0;ComPtr<IMFSample> sample;};
class Callback final:public Microsoft::WRL::RuntimeClass<Microsoft::WRL::RuntimeClassFlags<Microsoft::WRL::ClassicCom>,IMFSourceReaderCallback>{
    std::mutex mutex_;Packet packet_;
public:
    HANDLE ready=CreateEventW(nullptr,FALSE,FALSE,nullptr);
    ~Callback(){if(ready)CloseHandle(ready);}
    STDMETHODIMP OnReadSample(HRESULT h,DWORD,DWORD flags,LONGLONG pts,IMFSample* sample)override{
        {std::lock_guard lock(mutex_);packet_={h,flags,pts,nowSeconds(),sample};}
        SetEvent(ready);return S_OK;
    }
    STDMETHODIMP OnFlush(DWORD)override{return S_OK;}
    STDMETHODIMP OnEvent(DWORD,IMFMediaEvent*)override{return S_OK;}
    Packet take(){std::lock_guard lock(mutex_);return std::move(packet_);}
};
struct Config {std::wstring name,out;unsigned w=1280,h=720,fps=120,seconds=15;bool list=false;};
unsigned number(const std::wstring& s){size_t used=0;unsigned long n=std::stoul(s,&used);if(used!=s.size()||n>10000)throw std::invalid_argument("number");return static_cast<unsigned>(n);}
bool exact(IMFMediaType* type,const Config& c){UINT32 w=0,h=0,n=0,d=0;return SUCCEEDED(MFGetAttributeSize(type,MF_MT_FRAME_SIZE,&w,&h))&&SUCCEEDED(MFGetAttributeRatio(type,MF_MT_FRAME_RATE,&n,&d))&&w==c.w&&h==c.h&&s7probe::exactFrameRate(n,d,c.fps);}
ComPtr<IMFActivate> choose(const Config& c){
    ComPtr<IMFAttributes> a;check(MFCreateAttributes(&a,1));check(a->SetGUID(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE,MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID));
    IMFActivate** devices=nullptr;UINT32 count=0;check(MFEnumDeviceSources(a.Get(),&devices,&count));
    struct Cleanup {IMFActivate** p;UINT32 n;~Cleanup(){for(UINT32 i=0;i<n;++i)p[i]->Release();CoTaskMemFree(p);}} cleanup{devices,count};
    ComPtr<IMFActivate> selected;unsigned matches=0;
    for(UINT32 i=0;i<count;++i){wchar_t* raw=nullptr;UINT32 len=0;if(SUCCEEDED(devices[i]->GetAllocatedString(MF_DEVSOURCE_ATTRIBUTE_FRIENDLY_NAME,&raw,&len))){
        std::wstring name(raw,len);CoTaskMemFree(raw);if(c.list)std::wcout<<name<<L'\n';if(name==c.name){selected=devices[i];++matches;}}
    }
    if(c.list)return {};
    if(!matches)throw Problem{"device_not_found",HRESULT_FROM_WIN32(ERROR_NOT_FOUND)};
    if(matches!=1)throw Problem{"ambiguous_device",E_INVALIDARG};return selected;
}
struct Result {const char* status="capture_error";HRESULT hr=S_OK;bool advertised=false;uint64_t empty=0;unsigned flags=0;s7probe::FpsMetrics metrics;};
Result capture(const Config& c){
    Result result;
    try{
        auto device=choose(c);ComPtr<IMFMediaSource> source;check(device->ActivateObject(IID_PPV_ARGS(&source)));
        struct Shutdown {IMFMediaSource* p;~Shutdown(){p->Shutdown();}} shutdown{source.Get()};
        auto callback=Microsoft::WRL::Make<Callback>();if(!callback||!callback->ready)throw Problem{"allocation_error",E_OUTOFMEMORY};
        ComPtr<IMFAttributes> options;check(MFCreateAttributes(&options,4));check(options->SetUnknown(MF_SOURCE_READER_ASYNC_CALLBACK,callback.Get()));
        check(options->SetUINT32(MF_LOW_LATENCY,TRUE));check(options->SetUINT32(MF_READWRITE_ENABLE_HARDWARE_TRANSFORMS,TRUE));
        // No scaling/video processor. The native source must advertise the exact request.
        check(options->SetUINT32(MF_SOURCE_READER_ENABLE_VIDEO_PROCESSING,FALSE));
        ComPtr<IMFSourceReader> reader;check(MFCreateSourceReaderFromMediaSource(source.Get(),options.Get(),&reader));
        check(reader->SetStreamSelection(static_cast<DWORD>(MF_SOURCE_READER_ALL_STREAMS),FALSE));check(reader->SetStreamSelection(Video,TRUE));
        ComPtr<IMFMediaType> native;
        for(DWORD i=0;i<4096;++i){ComPtr<IMFMediaType> t;HRESULT hr=reader->GetNativeMediaType(Video,i,&t);if(hr==MF_E_NO_MORE_TYPES)break;check(hr);
            if(exact(t.Get(),c)){native=t;GUID subtype{};check(t->GetGUID(MF_MT_SUBTYPE,&subtype));if(subtype==MFVideoFormat_NV12)break;}}
        if(!native){result.status="not_advertised";return result;}
        result.advertised=true;check(reader->SetCurrentMediaType(Video,nullptr,native.Get()),"native_type_rejected");
        ComPtr<IMFMediaType> decoded;check(MFCreateMediaType(&decoded));check(decoded->SetGUID(MF_MT_MAJOR_TYPE,MFMediaType_Video));check(decoded->SetGUID(MF_MT_SUBTYPE,MFVideoFormat_NV12));
        check(MFSetAttributeSize(decoded.Get(),MF_MT_FRAME_SIZE,c.w,c.h));
        // Keep the EXACT accepted source interval. UVC high-FPS intervals use
        // 100 ns units; forcing fps/1 here can accidentally request conversion.
        UINT32 sourceN=0,sourceD=0;check(MFGetAttributeRatio(native.Get(),MF_MT_FRAME_RATE,&sourceN,&sourceD));
        check(MFSetAttributeRatio(decoded.Get(),MF_MT_FRAME_RATE,sourceN,sourceD));
        check(reader->SetCurrentMediaType(Video,nullptr,decoded.Get()),"decode_unavailable");
        ComPtr<IMFMediaType> current;check(reader->GetCurrentMediaType(Video,&current));GUID subtype{};check(current->GetGUID(MF_MT_SUBTYPE,&subtype));
        if(!exact(current.Get(),c)||subtype!=MFVideoFormat_NV12)throw Problem{"type_mismatch",MF_E_INVALIDMEDIATYPE};
        const double hardEnd=nowSeconds()+c.seconds+10;double begin=-1,end=hardEnd;
        while(nowSeconds()<end){
            check(reader->ReadSample(Video,0,nullptr,nullptr,nullptr,nullptr));
            const double remaining=std::min(end,hardEnd)-nowSeconds();
            if(remaining<=0)break;
            const DWORD timeout=static_cast<DWORD>(std::clamp(remaining*1000.0,1.0,3000.0));
            DWORD wait=WaitForSingleObject(callback->ready,timeout);
            if(wait==WAIT_TIMEOUT&&begin>=0&&nowSeconds()>=end)break;
            if(wait!=WAIT_OBJECT_0)throw Problem{"sample_timeout",HRESULT_FROM_WIN32(ERROR_TIMEOUT)};
            Packet p=callback->take();check(p.status);result.flags|=p.flags;
            if(p.flags&(MF_SOURCE_READERF_ERROR|MF_SOURCE_READERF_ENDOFSTREAM|MF_SOURCE_READERF_CURRENTMEDIATYPECHANGED|MF_SOURCE_READERF_NATIVEMEDIATYPECHANGED))throw Problem{"stream_changed_or_ended",E_FAIL};
            if(!p.sample){++result.empty;continue;}DWORD bytes=0;check(p.sample->GetTotalLength(&bytes));
            if(uint64_t(bytes)<uint64_t(c.w)*c.h*3/2)throw Problem{"short_nv12_sample",E_FAIL};
            if(begin<0){begin=p.arrival+2.0;end=std::min(begin+c.seconds,hardEnd);}
            if(p.arrival>=begin&&p.arrival<end)result.metrics.add(p.pts,p.arrival,c.fps);
        }
        check(reader->Flush(static_cast<DWORD>(MF_SOURCE_READER_ALL_STREAMS)));
        result.status=result.metrics.rateObserved(c.fps,c.seconds)?"rate_observed":"insufficient_rate_or_timing";
    }catch(const Problem& p){result.status=p.status;result.hr=p.hr;}
    return result;
}
std::string json(const Config& c,const Result& r){
    std::ostringstream o;o<<std::fixed<<std::setprecision(6);
    o<<"{\n  \"schema\": \"S7_WEBCAM_FPS_1\",\n  \"status\": \""<<r.status<<"\",\n  \"hresult\": "<<static_cast<uint32_t>(r.hr)
      <<",\n  \"width\": "<<c.w<<",\n  \"height\": "<<c.h<<",\n  \"requested_fps\": "<<c.fps<<",\n  \"requested_seconds\": "<<c.seconds
      <<",\n  \"warmup_seconds\": 2,\n  \"native_mode_advertised\": "<<(r.advertised?"true":"false")
      <<",\n  \"decoded_frames\": "<<r.metrics.frames<<",\n  \"wall_fps\": "<<r.metrics.wallFPS()<<",\n  \"pts_fps\": "<<r.metrics.ptsFPS()
      <<",\n  \"arrival_p95_ms\": "<<r.metrics.percentile(.95)<<",\n  \"arrival_p99_ms\": "<<r.metrics.percentile(.99)<<",\n  \"arrival_max_ms\": "<<r.metrics.percentile(1)
      <<",\n  \"nonmonotonic_samples\": "<<r.metrics.nonmonotonic<<",\n  \"estimated_missing_from_pts\": "<<r.metrics.estimatedMissing
      <<",\n  \"empty_callbacks\": "<<r.empty<<",\n  \"stream_flags\": "<<r.flags
      <<",\n  \"sensor_unique_frames_verified\": false,\n  \"automatically_unlocks_modes\": false,\n  \"video_saved\": false\n}\n";
    return o.str();
}
void saveNew(const std::wstring& path,const std::string& bytes){
    HANDLE f=CreateFileW(path.c_str(),GENERIC_WRITE,0,nullptr,CREATE_NEW,FILE_ATTRIBUTE_NORMAL,nullptr);
    if(f==INVALID_HANDLE_VALUE)throw Problem{"report_create_error",HRESULT_FROM_WIN32(GetLastError())};
    DWORD written=0;BOOL ok=WriteFile(f,bytes.data(),static_cast<DWORD>(bytes.size()),&written,nullptr);DWORD err=ok?ERROR_SUCCESS:GetLastError();
    if(ok)ok=FlushFileBuffers(f);CloseHandle(f);if(!ok||written!=bytes.size())throw Problem{"report_write_error",err?HRESULT_FROM_WIN32(err):E_FAIL};
}
int wmain(int argc,wchar_t** argv){
    Config c;
    try{for(int i=1;i<argc;++i){std::wstring a=argv[i];if(a==L"--list"){c.list=true;continue;}if(i+1==argc)throw std::invalid_argument("missing value");std::wstring v=argv[++i];
        if(a==L"--name")c.name=v;else if(a==L"--out")c.out=v;else if(a==L"--fps")c.fps=number(v);else if(a==L"--width")c.w=number(v);else if(a==L"--height")c.h=number(v);else if(a==L"--seconds")c.seconds=number(v);else throw std::invalid_argument("option");}
        if(!c.list&&(c.name.empty()||c.out.empty()||c.w<160||c.w>2560||c.h<120||c.h>1440||c.w%2||c.h%2||c.seconds<5||c.seconds>120||(c.fps!=30&&c.fps!=60&&c.fps!=120&&c.fps!=240)))throw std::invalid_argument("range");
        if(!c.list&&std::filesystem::exists(c.out))throw std::invalid_argument("report already exists");
    }catch(const std::exception&){std::cerr<<"Usage: camera-fps --list OR --name <exact camera name> --out <new.json> [--width 1280 --height 720 --fps 120 --seconds 15]\n";return 2;}
    HRESULT co=CoInitializeEx(nullptr,COINIT_MULTITHREADED);if(FAILED(co))return 2;
    HRESULT mf=MFStartup(MF_VERSION);if(FAILED(mf)){CoUninitialize();return 2;}
    int code=0;
    try{if(c.list){choose(c);}else{Result r=capture(c);auto text=json(c,r);saveNew(c.out,text);std::cout<<text;code=std::string(r.status)=="rate_observed"?0:1;}}
    catch(const Problem& p){std::cerr<<p.status<<" HRESULT=0x"<<std::hex<<static_cast<uint32_t>(p.hr)<<'\n';code=2;}
    catch(const std::exception&){std::cerr<<"Allocation/filesystem error\n";code=2;}
    MFShutdown();CoUninitialize();return code;
}
