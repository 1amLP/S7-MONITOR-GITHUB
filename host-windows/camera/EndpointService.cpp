#include "EndpointService.h"
#include "Camera.h"
#include "Physical.h"
#include "PackageUpdate.h"
#include "VirtualRegistration.h"
#include "AudioVisibility.h"
#include "Sniper.h"
#include "SniperMappingLock.h"
#include "TransportBinding.h"
#include "../monitor/HostWindows.h"
#include "../EndpointPolicy.h"
#include <cfgmgr32.h>
#include <devpkey.h>
#include <devguid.h>
#include <mmdeviceapi.h>
#include <functiondiscoverykeys_devpkey.h>
#include <string_view>
#include <condition_variable>
#include <thread>

namespace s7camera { std::atomic<long> objects{0}; }

namespace {
using namespace s7camera;
const wchar_t* ServiceName=L"S7EndpointSync";
bool bootstrapOnly=false;
bool developmentHostPackage=false;
using namespace s7endpoint;

class EndpointChannel {
    Preference device_{3,17};
public:
    uint32_t release=0;
    GUID const& containerId()const{return device_.containerId();}
    uint32_t endpoints(HANDLE cancel){
        std::vector<uint8_t> request(17);request[0]=10;
        const auto b=transferFeature(device_.handle(),std::move(request),false,cancel);
        uint32_t flags=0,reserved=0;std::memcpy(&flags,b.data()+9,4);std::memcpy(&reserved,b.data()+13,4);
        if(b[0]!=10||std::memcmp(b.data()+1,"S7E1",4)||(b[5]!=1&&b[5]!=2)||b[6]||b[7]!=16||b[8]||(b[5]==1&&reserved)||!validFlags(flags))throw E_INVALIDARG;
        release=reserved;
        return flags;
    }
    void endpointStatus(uint32_t wanted,uint32_t known,uint32_t applied,HRESULT error,HANDLE cancel){
        std::vector<uint8_t> b(17);b[0]=10;
        const uint32_t words[]{wanted,known,applied,uint32_t(error)};std::memcpy(b.data()+1,words,sizeof(words));
        transferFeature(device_.handle(),std::move(b),true,cancel);
    }
};
SERVICE_STATUS_HANDLE serviceHandle=nullptr;
HANDLE stopEvent=nullptr;

void status(DWORD state,DWORD error=NO_ERROR){
    SERVICE_STATUS serviceStatus{};
    serviceStatus.dwServiceType=SERVICE_WIN32_OWN_PROCESS;
    serviceStatus.dwCurrentState=state;serviceStatus.dwWin32ExitCode=error;
    serviceStatus.dwControlsAccepted=state==SERVICE_RUNNING?SERVICE_ACCEPT_STOP|SERVICE_ACCEPT_SHUTDOWN:0;
    serviceStatus.dwWaitHint=state==SERVICE_STOP_PENDING?15000:0;
    SetServiceStatus(serviceHandle,&serviceStatus);
}
DWORD WINAPI control(DWORD code,DWORD,void*,void*){
    if(code==SERVICE_CONTROL_STOP||code==SERVICE_CONTROL_SHUTDOWN){status(SERVICE_STOP_PENDING);SetEvent(stopEvent);}
    return NO_ERROR;
}
void cmcheck(CONFIGRET result){if(result!=CR_SUCCESS)throw HRESULT_FROM_WIN32(CM_MapCrToWin32Err(result,ERROR_GEN_FAILURE));}
std::wstring upper(std::wstring s){for(auto& c:s)c=static_cast<wchar_t>(towupper(c));return s;}
std::wstring nodeId(DEVINST node){
    wchar_t id[MAX_DEVICE_ID_LEN]{};cmcheck(CM_Get_Device_IDW(node,id,MAX_DEVICE_ID_LEN,0));return upper(id);
}
bool sameContainer(DEVINST node,GUID const& wanted){
    GUID found{};DEVPROPTYPE type=0;ULONG bytes=sizeof(found);
    const auto cr=CM_Get_DevNode_PropertyW(node,&DEVPKEY_Device_ContainerId,&type,reinterpret_cast<BYTE*>(&found),&bytes,0);
    return cr==CR_SUCCESS&&type==DEVPROP_TYPE_GUID&&bytes==sizeof(found)&&IsEqualGUID(found,wanted);
}
std::vector<std::wstring> idsFor(DEVINST node,DEVPROPKEY const& key){
    wchar_t raw[4096]{};ULONG bytes=sizeof(raw);DEVPROPTYPE type=0;
    const auto cr=CM_Get_DevNode_PropertyW(node,&key,&type,reinterpret_cast<BYTE*>(raw),&bytes,0);
    if(cr==CR_NO_SUCH_VALUE)return {};
    cmcheck(cr);if(type!=DEVPROP_TYPE_STRING_LIST||bytes>sizeof(raw)||bytes%sizeof(wchar_t))throw E_INVALIDARG;
    std::vector<std::wstring> ids;const size_t end=bytes/sizeof(wchar_t);
    for(size_t start=0;start<end&&raw[start];){
        size_t finish=start;while(finish<end&&raw[finish])++finish;
        if(finish==end)throw E_INVALIDARG;
        ids.push_back(upper(std::wstring(raw+start,finish-start)));start=finish+1;
    }
    return ids;
}
std::vector<std::wstring> compatible(DEVINST node){return idsFor(node,DEVPKEY_Device_CompatibleIds);}
// Only leaf devnodes are disabled. Never disable the composite, USB control,
// UAC adapter, or shared HID parent that also carries camera/volume reports.
bool setNode(DEVINST node,bool enabled,GUID const& container){
    if(!sameContainer(node,container))throw E_ACCESSDENIED;
    ULONG flags=0,problem=0;cmcheck(CM_Get_DevNode_Status(&flags,&problem,node,0));
    const bool disabled=(flags&DN_HAS_PROBLEM)&&problem==CM_PROB_DISABLED;
    if(enabled&&disabled)cmcheck(CM_Enable_DevNode(node,0));
    else if(!enabled&&!disabled)cmcheck(CM_Disable_DevNode(node,CM_DISABLE_UI_NOT_OK));
    cmcheck(CM_Get_DevNode_Status(&flags,&problem,node,0));
    if(enabled)return (flags&DN_STARTED)&&!(flags&DN_HAS_PROBLEM);
    return (flags&DN_HAS_PROBLEM)&&problem==CM_PROB_DISABLED;
}
bool setHidOrCamera(uint32_t bit,bool enabled,GUID const& container){
    const auto set=SetupDiGetClassDevsW(nullptr,nullptr,nullptr,DIGCF_PRESENT|DIGCF_ALLCLASSES);
    if(set==INVALID_HANDLE_VALUE)throw HRESULT_FROM_WIN32(GetLastError());
    struct Guard{HDEVINFO set;~Guard(){SetupDiDestroyDeviceInfoList(set);}}guard{set};
    std::vector<DEVINST> selected;
    for(DWORD i=0;;++i){
        SP_DEVINFO_DATA node{sizeof(node)};
        if(!SetupDiEnumDeviceInfo(set,i,&node)){if(GetLastError()==ERROR_NO_MORE_ITEMS)break;throw HRESULT_FROM_WIN32(GetLastError());}
        if(!sameContainer(node.DevInst,container))continue;
        const auto id=nodeId(node.DevInst);if(!nativeFunction(id))continue;
        auto ids=compatible(node.DevInst);
        const auto hardware=idsFor(node.DevInst,DEVPKEY_Device_HardwareIds);
        ids.insert(ids.end(),hardware.begin(),hardware.end());
        const bool match=std::any_of(ids.begin(),ids.end(),[&](auto const& value){
            return matches(bit,id,value);
        });
        if(match)selected.push_back(node.DevInst);
    }
    if(selected.empty()&&!enabled)return true;
    if(selected.size()!=1)throw HRESULT_FROM_WIN32(selected.empty()?ERROR_NOT_FOUND:ERROR_DUP_NAME);
    return setNode(selected[0],enabled,container);
}
bool nativeAudioPresent(GUID const& container){
    const auto set=SetupDiGetClassDevsW(&GUID_DEVCLASS_MEDIA,nullptr,nullptr,DIGCF_PRESENT);
    if(set==INVALID_HANDLE_VALUE)throw HRESULT_FROM_WIN32(GetLastError());
    struct Guard{HDEVINFO set;~Guard(){SetupDiDestroyDeviceInfoList(set);}}guard{set};
    unsigned found=0;
    for(DWORD i=0;;++i){
        SP_DEVINFO_DATA node{sizeof(node)};
        if(!SetupDiEnumDeviceInfo(set,i,&node)){if(GetLastError()==ERROR_NO_MORE_ITEMS)break;throw HRESULT_FROM_WIN32(GetLastError());}
        if(!sameContainer(node.DevInst,container))continue;
        const auto id=nodeId(node.DevInst);
        if(!id.starts_with(L"USB\\")||!nativeFunction(id))continue;
        const auto ids=compatible(node.DevInst);
        if(std::any_of(ids.begin(),ids.end(),[](auto const& v){return v.starts_with(L"USB\\CLASS_01&");}))++found;
    }
    if(found>1)throw HRESULT_FROM_WIN32(ERROR_DUP_NAME);
    return found==1;
}

bool setAudio(uint32_t bit,bool enabled,GUID const& container){
    if(!nativeAudioPresent(container)){if(!enabled)return true;throw HRESULT_FROM_WIN32(ERROR_NOT_FOUND);}
    ComPtr<IMMDeviceEnumerator> enumerator;check(CoCreateInstance(__uuidof(MMDeviceEnumerator),nullptr,CLSCTX_INPROC_SERVER,IID_PPV_ARGS(&enumerator)));
    ComPtr<IMMDeviceCollection> items;check(enumerator->EnumAudioEndpoints(bit==Speaker?eRender:eCapture,DEVICE_STATEMASK_ALL,&items));
    UINT count=0;check(items->GetCount(&count));std::vector<DEVINST> selected;std::wstring endpointId;
    for(UINT i=0;i<count;++i){
        ComPtr<IMMDevice> device;check(items->Item(i,&device));DWORD currentState=0;check(device->GetState(&currentState));
        if(currentState!=DEVICE_STATE_ACTIVE&&currentState!=DEVICE_STATE_DISABLED)continue;
        ComPtr<IPropertyStore> properties;check(device->OpenPropertyStore(STGM_READ,&properties));
        const PROPERTYKEY containerKey{DEVPKEY_Device_ContainerId.fmtid,DEVPKEY_Device_ContainerId.pid};
        PROPVARIANT endpointContainer{};
        const auto containerResult=properties->GetValue(containerKey,&endpointContainer);
        const bool owned=SUCCEEDED(containerResult)&&endpointContainer.vt==VT_CLSID&&endpointContainer.puuid&&IsEqualGUID(*endpointContainer.puuid,container);
        PropVariantClear(&endpointContainer);
        if(!owned)continue;
        PROPVARIANT value{};const auto result=properties->GetValue(PKEY_Device_InstanceId,&value);
        std::wstring id;if(SUCCEEDED(result)&&value.vt==VT_LPWSTR&&value.pwszVal)id=upper(value.pwszVal);PropVariantClear(&value);
        wchar_t* rawEndpoint=nullptr;check(device->GetId(&rawEndpoint));
        const std::wstring currentEndpoint=rawEndpoint;CoTaskMemFree(rawEndpoint);
        // This PC returns VT_EMPTY for PKEY_Device_InstanceId. Resolve the
        // opaque MMDevice ID in Windows' observed SWD endpoint namespace; the
        // existing devnode, container and USB parent must all agree below.
        if(id.empty())id=L"SWD\\MMDEVAPI\\"+upper(currentEndpoint);
        if(!id.starts_with(L"SWD\\MMDEVAPI\\"))continue;
        DEVINST node=0;auto cr=CM_Locate_DevNodeW(&node,id.data(),CM_LOCATE_DEVNODE_PHANTOM);
        // PolicyConfig can remove a disabled endpoint's SWD node. Core Audio
        // still owns its ID and container; a missing leaf must remain re-enableable.
        if(cr==CR_NO_SUCH_DEVNODE)node=0;
        else{cmcheck(cr);if(!sameContainer(node,container))continue;}
        selected.push_back(node);endpointId=currentEndpoint;
    }
    if(selected.empty()&&!enabled)return true;
    if(selected.size()!=1)throw HRESULT_FROM_WIN32(selected.empty()?ERROR_NOT_FOUND:ERROR_DUP_NAME);
    // Undo the old leaf-only PnP disable. It does not disable Core Audio, and
    // leaving it in place prevents a later Enabled On from restoring the node.
    ULONG flags=0,problem=0;
    if(selected[0]&&CM_Get_DevNode_Status(&flags,&problem,selected[0],0)==CR_SUCCESS&&
       (flags&DN_HAS_PROBLEM)&&problem==CM_PROB_DISABLED)cmcheck(CM_Enable_DevNode(selected[0],0));
    ComPtr<IMMDevice> endpoint;check(enumerator->GetDevice(endpointId.c_str(),&endpoint));
    DWORD state=0;check(endpoint->GetState(&state));
    if((enabled&&state!=DEVICE_STATE_ACTIVE)||(!enabled&&state!=DEVICE_STATE_DISABLED))
        setAudioVisibility(endpointId.c_str(),enabled);
    // Success requires Core Audio to publish the requested state, not a PnP
    // return code. The second check covers the list used by Windows' picker.
    check(endpoint->GetState(&state));
    if(state!=DWORD(enabled?DEVICE_STATE_ACTIVE:DEVICE_STATE_DISABLED))return false;
    ComPtr<IMMDeviceCollection> active;check(enumerator->EnumAudioEndpoints(bit==Speaker?eRender:eCapture,DEVICE_STATE_ACTIVE,&active));
    check(active->GetCount(&count));bool visible=false;
    for(UINT i=0;i<count;++i){
        ComPtr<IMMDevice> device;check(active->Item(i,&device));wchar_t* raw=nullptr;check(device->GetId(&raw));
        visible=visible||endpointId==raw;CoTaskMemFree(raw);
    }
    return visible==enabled;
}
void virtualCamera(selection::Sensor sensor,bool enabled){
    const auto name=endpointName(sensor);
    if(registeredName(name)==enabled)return;
    ComPtr<IMFVirtualCamera> camera;
    check(MFCreateVirtualCamera(MFVirtualCameraType_SoftwareCameraSource,MFVirtualCameraLifetime_System,MFVirtualCameraAccess_AllUsers,name,endpointClassText(sensor),nullptr,0,&camera));
    const auto result=guarded([&]{
        if(enabled){check(camera->Start(nullptr));}
        else check(removeCamera(camera.Get(),name));
    });
    camera->Shutdown();check(result);
    if(registeredName(name)!=enabled)throw HRESULT_FROM_WIN32(ERROR_RETRY);
}
void setCamera(bool enabled,GUID const& container){
    if(enabled){
        const auto binding=cameraBinding(true);
        if(!binding.present||!binding.privateTransport||!binding.matches)throw HRESULT_FROM_WIN32(binding.reboot?ERROR_SUCCESS_REBOOT_REQUIRED:ERROR_RETRY);
    }
    if(enabled&&!setHidOrCamera(Camera,true,container))throw HRESULT_FROM_WIN32(ERROR_RETRY);
    for(auto sensor:{selection::Sensor::Rear,selection::Sensor::Front})virtualCamera(sensor,enabled);
    if(!enabled&&!setHidOrCamera(Camera,false,container))throw HRESULT_FROM_WIN32(ERROR_RETRY);
}
class CameraWorker {
    std::mutex mutex_;std::condition_variable changed_;
    bool stop_=false,valid_=false,wanted_=false,known_=false;
    GUID container_{};uint64_t revision_=0;HRESULT error_=S_OK,terminalError_=S_OK;
    std::thread thread_;
    void loop()noexcept{
        const HRESULT com=CoInitializeEx(nullptr,COINIT_MULTITHREADED);
        if(FAILED(com)){std::lock_guard lock(mutex_);terminalError_=com;return;}
        uint64_t done=0;
        for(;;){
            GUID container{};bool wanted=false;uint64_t revision=0;
            {
                std::unique_lock lock(mutex_);
                changed_.wait_for(lock,std::chrono::seconds(2),[&]{return stop_||(valid_&&revision_!=done);});
                if(stop_)break;if(!valid_)continue;
                container=container_;wanted=wanted_;revision=revision_;
            }
            HRESULT result=guarded([&]{setCamera(wanted,container);});
            {
                std::lock_guard lock(mutex_);done=revision;
                if(revision_==revision){known_=SUCCEEDED(result);error_=result;}
            }
        }
        CoUninitialize();
    }
public:
    CameraWorker():thread_([this]{loop();}){}
    ~CameraWorker(){ {std::lock_guard lock(mutex_);stop_=true;} changed_.notify_one();thread_.join(); }
    HRESULT request(bool wanted,GUID const& container,bool& known){
        std::lock_guard lock(mutex_);
        if(FAILED(terminalError_)){known=false;return terminalError_;}
        if(!valid_||wanted_!=wanted||!IsEqualGUID(container_,container)){
            valid_=true;wanted_=wanted;container_=container;++revision_;known_=false;error_=S_OK;changed_.notify_one();
        }
        known=known_;return error_;
    }
    void pause(){std::lock_guard lock(mutex_);valid_=false;known_=false;++revision_;}
};
void run(){
    std::thread sniper([&]{runSniperBroker(stopEvent);});
    struct Join{std::thread& t;~Join(){SetEvent(stopEvent);t.join();}}join{sniper};
    // MF activation can be slow. It never owns the USB polling/audio/HID loop.
    CameraWorker camera;
    PhonePackageUpdate updater;
    std::unique_ptr<EndpointChannel> usb;uint32_t last=UINT32_MAX;
    ULONGLONG next=0;HRESULT lastError=S_OK;GUID container{};
    while(WaitForSingleObject(stopEvent,250)==WAIT_TIMEOUT){
        try{
            if(!usb){usb=std::make_unique<EndpointChannel>();last=UINT32_MAX;container=usb->containerId();}
            const auto desired=usb->endpoints(stopEvent);
            const HRESULT packageState=developmentHostPackage?S_FALSE:updater.tick(usb->release);
            if(FAILED(packageState)){
                usb->endpointStatus(desired,0,0,packageState,stopEvent);
                continue;
            }
            if(last==desired&&GetTickCount64()<next)continue;
            uint32_t known=0,applied=0;HRESULT error=S_OK;
            for(auto bit:{Speaker,s7endpoint::Microphone,Touch,Pad,SniperTouch,Camera}){
                try{
                    const bool enabled=(desired&bit)!=0;
                    bool done=false;
                    if(bit==Camera){check(camera.request(enabled,container,done));}
                    else if(bit==Speaker||bit==s7endpoint::Microphone)done=setAudio(bit,enabled,container);
                    else if(bit==SniperTouch){std::lock_guard<std::mutex> guard(s7::sniperMappingMutex());done=setHidOrCamera(bit,enabled,container);}
                    else done=setHidOrCamera(bit,enabled,container);
                    if(done){known|=bit;if(enabled)applied|=bit;}else if(SUCCEEDED(error))error=HRESULT_FROM_WIN32(ERROR_RETRY);
                }catch(HRESULT hr){if(SUCCEEDED(error))error=hr;}
            }
            usb->endpointStatus(desired,known|(usb->release&&!developmentHostPackage?0x80000000u:0),applied,error,stopEvent);
            last=desired;next=GetTickCount64()+(FAILED(error)?1000:5000);
            if(error!=lastError){s7::log("Endpoint synchronization",error);lastError=error;}
        }catch(HRESULT hr){
            if(hr!=lastError){s7::log("Endpoint HID channel",hr);lastError=hr;}
            // A timeout is not Off. Do not touch endpoint state on failed reads.
            updater.disconnected();camera.pause();usb.reset();WaitForSingleObject(stopEvent,1000);
        }
    }
}
void runBootstrap(){
    PhonePackageUpdate updater;
    std::unique_ptr<EndpointChannel> channel;
    unsigned absent=0;
    while(WaitForSingleObject(stopEvent,500)==WAIT_TIMEOUT){
        try{
            if(!channel)channel=std::make_unique<EndpointChannel>();
            const auto desired=channel->endpoints(stopEvent);absent=0;
            if(channel->release==0){s7::log("S7 has no delivery package",E_NOTIMPL);return;}
            const HRESULT result=updater.tick(channel->release);
            channel->endpointStatus(desired,result==S_OK?0x80000000u:0,0,result,stopEvent);
            if(result==S_OK)return;
        }catch(HRESULT hr){
            channel.reset();
            if(++absent>=4){s7::log("S7 bootstrap stopped without interface",hr);return;}
        }
    }
}
void WINAPI mainService(DWORD,wchar_t**){
    stopEvent=CreateEventW(nullptr,TRUE,FALSE,nullptr);
    if(!stopEvent)return;
    serviceHandle=RegisterServiceCtrlHandlerExW(ServiceName,control,nullptr);
    if(!serviceHandle){CloseHandle(stopEvent);return;}
    status(SERVICE_START_PENDING);
    HRESULT hr=CoInitializeEx(nullptr,COINIT_MULTITHREADED);
    if(SUCCEEDED(hr)){
        hr=MFStartup(MF_VERSION);
        if(SUCCEEDED(hr)){
            status(SERVICE_RUNNING);
            try{if(bootstrapOnly)runBootstrap();else run();}catch(...){hr=E_FAIL;}
            MFShutdown();
        }
        CoUninitialize();
    }
    status(SERVICE_STOPPED,FAILED(hr)?ERROR_SERVICE_SPECIFIC_ERROR:NO_ERROR);
    CloseHandle(stopEvent);
}
}
int runEndpointService(bool bootstrap,bool development){
    developmentHostPackage=development&&!bootstrap;
    bootstrapOnly=bootstrap;if(bootstrap)ServiceName=L"S7PackageBootstrap";
    SERVICE_TABLE_ENTRYW table[]={{const_cast<wchar_t*>(ServiceName),mainService},{nullptr,nullptr}};
    return StartServiceCtrlDispatcherW(table)?0:static_cast<int>(GetLastError());
}
