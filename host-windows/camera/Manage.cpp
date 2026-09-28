#include "Camera.h"
#include "Physical.h"
#include "EndpointService.h"
#include "VirtualRegistration.h"
#include "CaptureMetadata.h"
#include "TransportBinding.h"
#include <algorithm>
#include <iostream>
#include <mutex>
#include <string_view>
#include <wincodec.h>
using namespace s7camera;
struct ProbeTarget {selection::Sensor sensor;WebcamMode mode;GUID subtype;char const* label;bool legacy=false;};
uint32_t decimal(wchar_t const* value){
    if(!value||!*value)throw E_INVALIDARG;
    uint64_t number=0;
    for(auto p=value;*p;++p){if(*p<L'0'||*p>L'9')throw E_INVALIDARG;number=number*10+uint32_t(*p-L'0');if(number>UINT32_MAX)throw E_INVALIDARG;}
    return uint32_t(number);
}
ProbeTarget target(wchar_t** args){
    ProbeTarget t{};
    const std::wstring_view sensor=args[0],format=args[4];
    if(sensor==L"rear")t.sensor=selection::Sensor::Rear;
    else if(sensor==L"front")t.sensor=selection::Sensor::Front;
    else if(sensor==L"legacy"){t.sensor=selection::Sensor::Rear;t.legacy=true;}
    else throw E_INVALIDARG;
    t.mode={decimal(args[1]),decimal(args[2]),decimal(args[3])};
    if(!webcamEligible(t.mode)||!selection::bit(t.sensor,t.mode))throw MF_E_INVALIDMEDIATYPE;
    if(format==L"nv12"){t.subtype=MFVideoFormat_NV12;t.label="NV12";}
    else if(format==L"mjpeg"){t.subtype=MFVideoFormat_MJPG;t.label="MJPEG";}
    else throw E_INVALIDARG;
    return t;
}
struct Result final : ComObject<IMFSourceReaderCallback> {
    HANDLE ready=CreateEventW(nullptr,FALSE,FALSE,nullptr),flushed=CreateEventW(nullptr,FALSE,FALSE,nullptr);std::mutex mutex;HRESULT result=S_OK;ComPtr<IMFSample> frame;
    ~Result(){CloseHandle(ready);CloseHandle(flushed);}
    STDMETHODIMP OnReadSample(HRESULT hr,DWORD,DWORD,LONGLONG,IMFSample* sample) override{std::lock_guard lock(mutex);result=hr;frame=sample;SetEvent(ready);return S_OK;}
    STDMETHODIMP OnEvent(DWORD,IMFMediaEvent*) override{return S_OK;}
    STDMETHODIMP OnFlush(DWORD) override{SetEvent(flushed);return S_OK;}
    ComPtr<IMFSample> take(){std::lock_guard lock(mutex);check(result);return std::move(frame);}
};
void verifyJpegDimensions(std::vector<BYTE> const& jpeg,UINT32 width,UINT32 height){
    HGLOBAL memory=GlobalAlloc(GMEM_MOVEABLE,jpeg.size());if(!memory)throw E_OUTOFMEMORY;
    ComPtr<IStream> stream;HRESULT hr=CreateStreamOnHGlobal(memory,TRUE,&stream);
    if(FAILED(hr)){GlobalFree(memory);check(hr);}
    auto* data=GlobalLock(memory);if(!data)throw HRESULT_FROM_WIN32(GetLastError());
    std::memcpy(data,jpeg.data(),jpeg.size());GlobalUnlock(memory);
    ComPtr<IWICImagingFactory> factory;check(CoCreateInstance(CLSID_WICImagingFactory2,nullptr,CLSCTX_INPROC_SERVER,IID_PPV_ARGS(&factory)));
    ComPtr<IWICBitmapDecoder> decoder;check(factory->CreateDecoderFromStream(stream.Get(),nullptr,WICDecodeMetadataCacheOnDemand,&decoder));
    ComPtr<IWICBitmapFrameDecode> picture;check(decoder->GetFrame(0,&picture));UINT32 w=0,h=0;check(picture->GetSize(&w,&h));
    if(w!=width||h!=height)throw MF_E_INVALIDMEDIATYPE;
}
void testFormat(IMFMediaSource* source,GUID const& wanted,char const* label,WebcamMode requested={}){
    auto result=Make<Result>();ComPtr<IMFAttributes> options;check(MFCreateAttributes(&options,2));check(options->SetUnknown(MF_SOURCE_READER_ASYNC_CALLBACK,result.Get()));
    check(options->SetUINT32(MF_SOURCE_READER_DISCONNECT_MEDIASOURCE_ON_SHUTDOWN,TRUE));
    ComPtr<IMFSourceReader> reader;check(MFCreateSourceReaderFromMediaSource(source,options.Get(),&reader));
    ComPtr<IMFMediaType> type;
    for(DWORD i=0;;++i){
        ComPtr<IMFMediaType> candidate;HRESULT hr=reader->GetNativeMediaType(static_cast<DWORD>(MF_SOURCE_READER_FIRST_VIDEO_STREAM),i,&candidate);
        if(hr==MF_E_NO_MORE_TYPES)throw MF_E_INVALIDMEDIATYPE;check(hr);GUID subtype{};check(candidate->GetGUID(MF_MT_SUBTYPE,&subtype));
        if(subtype!=wanted)continue;
        if(requested.width){
            UINT32 w=0,h=0,n=0,d=0;
            if(FAILED(MFGetAttributeSize(candidate.Get(),MF_MT_FRAME_SIZE,&w,&h))||
               FAILED(MFGetAttributeRatio(candidate.Get(),MF_MT_FRAME_RATE,&n,&d))||
               !sameVisibleVideoFormat({w,h,n,d},{requested.width,requested.height,requested.fps,1}))continue;
        }
        type=std::move(candidate);break;
    }
    check(reader->SetCurrentMediaType(static_cast<DWORD>(MF_SOURCE_READER_FIRST_VIDEO_STREAM),nullptr,type.Get()));
    ComPtr<IMFMediaType> current;check(reader->GetCurrentMediaType(static_cast<DWORD>(MF_SOURCE_READER_FIRST_VIDEO_STREAM),&current));
    UINT32 w=0,h=0,n=0,d=0;GUID actual{};
    check(MFGetAttributeSize(current.Get(),MF_MT_FRAME_SIZE,&w,&h));check(MFGetAttributeRatio(current.Get(),MF_MT_FRAME_RATE,&n,&d));check(current->GetGUID(MF_MT_SUBTYPE,&actual));
    UINT32 wantedW=0,wantedH=0,wantedN=0,wantedD=0;
    check(MFGetAttributeSize(type.Get(),MF_MT_FRAME_SIZE,&wantedW,&wantedH));check(MFGetAttributeRatio(type.Get(),MF_MT_FRAME_RATE,&wantedN,&wantedD));
    if(actual!=wanted||!sameVisibleVideoFormat({w,h,n,d},{wantedW,wantedH,wantedN,wantedD})||uint64_t(w)*9!=uint64_t(h)*16)throw MF_E_INVALIDMEDIATYPE;
    std::cout<<"Virtual source "<<label<<' '<<w<<'x'<<h<<' '<<n<<'/'<<d<<" fps"<<std::endl;
    const int count=requested.width?30:3;LONGLONG first=-1,previous=-1;
    const auto deadline=GetTickCount64()+20000;
    for(int i=0;i<count;++i){
        check(reader->ReadSample(static_cast<DWORD>(MF_SOURCE_READER_FIRST_VIDEO_STREAM),0,nullptr,nullptr,nullptr,nullptr));
        const auto remaining=deadline-GetTickCount64();
        if(remaining>20000||WaitForSingleObject(result->ready,DWORD(std::min<ULONGLONG>(remaining,5000)))!=WAIT_OBJECT_0)throw HRESULT_FROM_WIN32(ERROR_TIMEOUT);
        auto frame=result->take();if(!frame)throw E_FAIL;DWORD size=0;check(frame->GetTotalLength(&size));
        LONGLONG stamp=0,duration=0;check(frame->GetSampleTime(&stamp));check(frame->GetSampleDuration(&duration));
        if(stamp<0||duration<=0||(previous>=0&&stamp<=previous))throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);
        if(first<0)first=stamp;previous=stamp;
        if(wanted==MFVideoFormat_NV12){if(size!=w*h*3/2)throw MF_E_INVALIDMEDIATYPE;}
        else{
            ComPtr<IMFMediaBuffer> data;check(frame->ConvertToContiguousBuffer(&data));BYTE* bytes=nullptr;DWORD length=0;check(data->Lock(&bytes,nullptr,&length));
            const bool jpeg=length>=4&&bytes[0]==0xff&&bytes[1]==0xd8&&bytes[length-2]==0xff&&bytes[length-1]==0xd9;
            std::vector<BYTE> jpegPayload;
            if(jpeg&&i==0)jpegPayload.assign(bytes,bytes+length);
            check(data->Unlock());if(!jpeg)throw MF_E_INVALIDMEDIATYPE;
            if(!jpegPayload.empty())verifyJpegDimensions(jpegPayload,w,h);
        }
        if(i==0||i+1==count)std::cout<<label<<" frame "<<i+1<<" bytes="<<size<<" time="<<stamp<<std::endl;
    }
    if(count>1&&previous>first)std::cout<<label<<" observed source cadence="<<double(count-1)*10000000.0/double(previous-first)<<" fps"<<std::endl;
    ResetEvent(result->flushed);check(reader->Flush(static_cast<DWORD>(MF_SOURCE_READER_ALL_STREAMS)));
    if(WaitForSingleObject(result->flushed,5000)!=WAIT_OBJECT_0)throw HRESULT_FROM_WIN32(ERROR_TIMEOUT);
}
void test(wchar_t const* path,selection::Sensor sensor,ProbeTarget const* targetMode=nullptr){
    auto module=LoadLibraryExW(path,nullptr,LOAD_WITH_ALTERED_SEARCH_PATH);if(!module)throw HRESULT_FROM_WIN32(GetLastError());
    auto factoryFunction=reinterpret_cast<HRESULT(__stdcall*)(REFCLSID,REFIID,void**)>(GetProcAddress(module,"DllGetClassObject"));
    if(!factoryFunction)throw E_NOINTERFACE;
    ComPtr<IClassFactory> factory;check(factoryFunction(endpointClass(sensor),IID_PPV_ARGS(&factory)));
    ComPtr<IMFActivate> activation;check(factory->CreateInstance(nullptr,IID_PPV_ARGS(&activation)));
    ComPtr<IMFMediaSource> source;check(activation->ActivateObject(IID_PPV_ARGS(&source)));
    // The capture stack may release activation before it releases the source.
    activation.Reset();
    HRESULT tested=guarded([&]{
        if(targetMode)testFormat(source.Get(),targetMode->subtype,targetMode->label,targetMode->mode);
        else{testFormat(source.Get(),MFVideoFormat_NV12,"NV12");testFormat(source.Get(),MFVideoFormat_MJPG,"MJPEG");}
    });
    source->Shutdown();source.Reset();factory.Reset();
    // The MF runtime may still release callback references asynchronously.
    check(tested);std::cout<<(targetMode?"PASS: selected source mode and frames. No installation.\n":"PASS: source lifetime and real NV12/MJPEG frames. No installation.\n");
}
void testRegistered(ProbeTarget const& targetMode){
    ComPtr<IMFAttributes> filter;check(MFCreateAttributes(&filter,1));
    check(filter->SetGUID(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE,MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID));
    IMFActivate** devices=nullptr;UINT32 count=0;check(MFEnumDeviceSources(filter.Get(),&devices,&count));
    struct List{IMFActivate** items;UINT32 count;~List(){for(UINT32 i=0;i<count;++i)items[i]->Release();CoTaskMemFree(items);}} list{devices,count};
    ComPtr<IMFActivate> chosen;
    for(UINT32 i=0;i<count;++i){
        wchar_t* name=nullptr;UINT32 length=0;
        if(FAILED(devices[i]->GetAllocatedString(MF_DEVSOURCE_ATTRIBUTE_FRIENDLY_NAME,&name,&length)))continue;
        const std::wstring found(name);CoTaskMemFree(name);
        if(found!=(targetMode.legacy?LegacyName:endpointName(targetMode.sensor)))continue;
        if(chosen)throw HRESULT_FROM_WIN32(ERROR_DUP_NAME);
        chosen=devices[i];
    }
    if(!chosen)throw HRESULT_FROM_WIN32(ERROR_NOT_FOUND);
    ComPtr<IMFMediaSource> source;check(chosen->ActivateObject(IID_PPV_ARGS(&source)));
    HRESULT tested=guarded([&]{testFormat(source.Get(),targetMode.subtype,targetMode.label,targetMode.mode);});
    source->Shutdown();check(tested);
    std::cout<<"PASS: registered Media Foundation camera produced selected mode. No installation.\n";
}
int runSniperDesktop();
int wmain(int argc,wchar_t** argv){
    if(argc==2&&std::wstring_view(argv[1])==L"desktop")return runSniperDesktop();
    if(argc==2&&std::wstring_view(argv[1])==L"service")return runEndpointService();
    if(argc==2&&std::wstring_view(argv[1])==L"service-dev")return runEndpointService(false,true);
    if(argc<2)return 2;check(CoInitializeEx(nullptr,COINIT_MULTITHREADED));check(MFStartup(MF_VERSION));
    HRESULT result=guarded([&]{
        std::wstring verb=argv[1];
        if(verb==L"transport-status"||verb==L"transport-bind"){
            const auto s=cameraBinding(verb==L"transport-bind");
            std::cout<<"{\"present\":"<<(s.present?"true":"false")<<",\"private\":"<<(s.privateTransport?"true":"false")
                <<",\"matches\":"<<(s.matches?"true":"false")<<",\"changed\":"<<(s.changed?"true":"false")
                <<",\"reboot_required\":"<<(s.reboot?"true":"false")<<"}"<<std::endl;
            return;
        }
        if(verb==L"list"||verb==L"describe"){
            ComPtr<IMFAttributes> filter;check(MFCreateAttributes(&filter,1));
            check(filter->SetGUID(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE,MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID));
            IMFActivate** items=nullptr;UINT32 count=0;check(MFEnumDeviceSources(filter.Get(),&items,&count));
            struct List{IMFActivate** p;UINT32 n;~List(){for(UINT32 i=0;i<n;++i)p[i]->Release();CoTaskMemFree(p);}}list{items,count};
            for(UINT32 i=0;i<count;++i){
                wchar_t* name=nullptr;UINT32 length=0;
                bool owned=false;
                if(SUCCEEDED(items[i]->GetAllocatedString(MF_DEVSOURCE_ATTRIBUTE_FRIENDLY_NAME,&name,&length))){
                    const std::wstring visible(name);owned=visible==std::wstring(Name)+L" (Windows Virtual Camera)"||visible==std::wstring(FrontName)+L" (Windows Virtual Camera)";
                    std::wcout<<name<<L'\n';CoTaskMemFree(name);
                }
                wchar_t* link=nullptr;
                if(SUCCEEDED(items[i]->GetAllocatedString(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_SYMBOLIC_LINK,&link,&length))){
                    std::wstring path(link);std::transform(path.begin(),path.end(),path.begin(),towlower);
                    owned=owned||s7UsbFunction(path);std::wcout<<link<<L'\n';CoTaskMemFree(link);
                }
                if(verb==L"describe"&&owned)describeCaptureMetadata(items[i]);
            }
            return;
        }
        if(verb==L"test"&&argc==3){test(argv[2],selection::Sensor::Rear);test(argv[2],selection::Sensor::Front);return;}
        if(verb==L"check-target"&&argc==7){
            auto selected=target(argv+2);
            std::wcout<<argv[2]<<L' '<<selected.mode.width<<L'x'<<selected.mode.height<<L'@'<<selected.mode.fps<<L' '<<argv[6]<<L" valid target\n";return;
        }
        if(verb==L"test"&&argc==8){auto selected=target(argv+3);if(selected.legacy)throw E_INVALIDARG;test(argv[2],selected.sensor,&selected);return;}
        if(verb==L"test-registered"&&argc==7){testRegistered(target(argv+2));return;}
        if(verb!=L"install"&&verb!=L"remove")throw E_INVALIDARG;
        for(auto sensor:{selection::Sensor::Rear,selection::Sensor::Front}){
            if(verb==L"remove"&&!registeredName(endpointName(sensor))){
                std::wcout<<endpointName(sensor)<<L": endpoint already absent"<<std::endl;continue;
            }
            std::wcout<<endpointName(sensor)<<L": create virtual camera registration"<<std::endl;
            ComPtr<IMFVirtualCamera> camera;
            check(MFCreateVirtualCamera(MFVirtualCameraType_SoftwareCameraSource,MFVirtualCameraLifetime_System,MFVirtualCameraAccess_AllUsers,endpointName(sensor),endpointClassText(sensor),nullptr,0,&camera));
            HRESULT action=guarded([&]{if(verb==L"install"){
                std::wcout<<endpointName(sensor)<<L": start registration"<<std::endl;
                check(camera->Start(nullptr));
            }else {std::wcout<<endpointName(sensor)<<L": remove registration"<<std::endl;check(removeCamera(camera.Get(),endpointName(sensor)));}});
            camera->Shutdown();check(action);
            std::wcout<<endpointName(sensor)<<L": "<<verb<<L" complete\n";
        }
        // Explicit upgrade cleanup of the former single endpoint only. Its
        // identity is keyed by the OLD friendlyName as well as the rear CLSID.
        if(verb==L"remove"&&registeredName(LegacyName)){
            ComPtr<IMFVirtualCamera> legacy;
            check(MFCreateVirtualCamera(MFVirtualCameraType_SoftwareCameraSource,MFVirtualCameraLifetime_System,MFVirtualCameraAccess_AllUsers,LegacyName,ClassText,nullptr,0,&legacy));
            HRESULT action=removeCamera(legacy.Get(),LegacyName);legacy->Shutdown();check(action);
        }
    });
    MFShutdown();CoUninitialize();if(FAILED(result)){std::cerr << "S7 camera HRESULT=0x" << std::hex << unsigned(result) << '\n';return 1;}return 0;
}
