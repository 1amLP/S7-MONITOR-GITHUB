#pragma once
#include "Preference.h"
#include "../EndpointPolicy.h"
#include <newdev.h>
#include <cfgmgr32.h>
#include <optional>

namespace s7camera {
struct CameraBindingStatus {bool present=false,privateTransport=false,matches=false,changed=false,reboot=false;};
inline std::wstring transportUpper(std::wstring text){for(auto& c:text)c=static_cast<wchar_t>(towupper(c));return text;}
inline std::vector<std::wstring> transportProperty(HDEVINFO set,SP_DEVINFO_DATA& node,DWORD key,DWORD expected){
    DWORD type=0,bytes=0;
    if(!SetupDiGetDeviceRegistryPropertyW(set,&node,key,&type,nullptr,0,&bytes)){
        const auto error=GetLastError();if(error==ERROR_INVALID_DATA)return {};
        if(error!=ERROR_INSUFFICIENT_BUFFER)throw HRESULT_FROM_WIN32(error);
    }
    if(bytes>65536||bytes%sizeof(wchar_t))throw E_INVALIDARG;
    std::vector<wchar_t> data(bytes/sizeof(wchar_t)+2);
    if(!SetupDiGetDeviceRegistryPropertyW(set,&node,key,&type,reinterpret_cast<BYTE*>(data.data()),bytes,nullptr))throw HRESULT_FROM_WIN32(GetLastError());
    if(type!=expected)throw E_INVALIDARG;
    std::vector<std::wstring> values;
    for(size_t i=0;i<data.size()&&data[i];){size_t end=i;while(end<data.size()&&data[end])++end;if(end==data.size())throw E_INVALIDARG;values.push_back(transportUpper(std::wstring(data.data()+i,end-i)));i=end+1;}
    return values;
}
// Bind only the camera leaf belonging to the HID-serial-verified S7 container.
// The driver is the inbox WinUSB/usbvideo driver, not a new kernel binary.
inline CameraBindingStatus cameraBinding(bool repair){
    std::optional<Preference> identity;
    try{identity.emplace();}catch(HRESULT hr){
        if(hr==HRESULT_FROM_WIN32(ERROR_DEVICE_NOT_CONNECTED)){CameraBindingStatus absent;absent.matches=true;return absent;}throw;
    }
    const auto container=identity->containerId();
    const auto set=SetupDiGetClassDevsW(nullptr,nullptr,nullptr,DIGCF_PRESENT|DIGCF_ALLCLASSES);
    if(set==INVALID_HANDLE_VALUE)throw HRESULT_FROM_WIN32(GetLastError());
    struct Guard{HDEVINFO s;~Guard(){SetupDiDestroyDeviceInfoList(s);}}guard{set};
    SP_DEVINFO_DATA chosen{};CameraBindingStatus result;
    for(DWORD i=0;;++i){
        SP_DEVINFO_DATA node{sizeof(node)};
        if(!SetupDiEnumDeviceInfo(set,i,&node)){if(GetLastError()==ERROR_NO_MORE_ITEMS)break;throw HRESULT_FROM_WIN32(GetLastError());}
        wchar_t instance[MAX_DEVICE_ID_LEN]{};
        if(!SetupDiGetDeviceInstanceIdW(set,&node,instance,MAX_DEVICE_ID_LEN,nullptr))continue;
        const auto id=transportUpper(instance);
        if(!s7endpoint::nativeFunction(id)||!id.starts_with(L"USB\\"))continue;
        if(!IsEqualGUID(containerForDevice(set,node),container))continue;
        const auto ids=transportProperty(set,node,SPDRP_COMPATIBLEIDS,REG_MULTI_SZ);
        bool vendor=false,uvc=false;
        for(auto const& value:ids){vendor=vendor||value==L"USB\\CLASS_FF&SUBCLASS_53&PROT_72";
            uvc=uvc||value.starts_with(L"USB\\CLASS_0E&SUBCLASS_01")||value.starts_with(L"USB\\CLASS_0E&SUBCLASS_03");}
        if(!vendor&&!uvc)continue;
        if(result.present)throw HRESULT_FROM_WIN32(ERROR_DUP_NAME);
        chosen=node;result.present=true;result.privateTransport=vendor;
    }
    if(!result.present){result.matches=true;return result;}
    const std::wstring expected=result.privateTransport?L"WINUSB":L"USBVIDEO";
    auto service=transportProperty(set,chosen,SPDRP_SERVICE,REG_SZ);
    result.matches=service.size()==1&&service[0]==expected;
    if(result.matches||!repair)return result;
    SP_DEVINSTALL_PARAMS_W params{sizeof(params)};
    if(!SetupDiGetDeviceInstallParamsW(set,&chosen,&params))throw HRESULT_FROM_WIN32(GetLastError());
    params.FlagsEx|=DI_FLAGSEX_ALLOWEXCLUDEDDRVS;
    if(!SetupDiSetDeviceInstallParamsW(set,&chosen,&params)||!SetupDiBuildDriverInfoList(set,&chosen,SPDIT_COMPATDRIVER))throw HRESULT_FROM_WIN32(GetLastError());
    struct Drivers{HDEVINFO s;SP_DEVINFO_DATA* n;~Drivers(){SetupDiDestroyDriverInfoList(s,n,SPDIT_COMPATDRIVER);}}drivers{set,&chosen};
    wchar_t system[MAX_PATH]{};if(!GetWindowsDirectoryW(system,MAX_PATH))throw HRESULT_FROM_WIN32(GetLastError());
    const std::wstring expectedInf=transportUpper(std::wstring(system)+L"\\INF\\"+(result.privateTransport?L"winusb.inf":L"usbvideo.inf"));
    bool found=false;SP_DRVINFO_DATA_W selected{sizeof(selected)};
    for(DWORD i=0;;++i){
        SP_DRVINFO_DATA_W info{sizeof(info)};
        if(!SetupDiEnumDriverInfoW(set,&chosen,SPDIT_COMPATDRIVER,i,&info)){if(GetLastError()==ERROR_NO_MORE_ITEMS)break;throw HRESULT_FROM_WIN32(GetLastError());}
        DWORD bytes=0;SetupDiGetDriverInfoDetailW(set,&chosen,&info,nullptr,0,&bytes);
        if(bytes<sizeof(SP_DRVINFO_DETAIL_DATA_W)||bytes>65536)continue;
        std::vector<BYTE> storage(bytes);auto detail=reinterpret_cast<SP_DRVINFO_DETAIL_DATA_W*>(storage.data());detail->cbSize=sizeof(*detail);
        if(!SetupDiGetDriverInfoDetailW(set,&chosen,&info,detail,bytes,nullptr))throw HRESULT_FROM_WIN32(GetLastError());
        if(transportUpper(detail->InfFileName)!=expectedInf)continue;
        if(!found||info.DriverVersion>selected.DriverVersion){selected=info;found=true;}
    }
    if(!found)throw HRESULT_FROM_WIN32(ERROR_NOT_FOUND);
    BOOL reboot=FALSE;
    if(!DiInstallDevice(nullptr,set,&chosen,&selected,0,&reboot))throw HRESULT_FROM_WIN32(GetLastError());
    result.changed=true;result.reboot=reboot!=FALSE;
    service=transportProperty(set,chosen,SPDRP_SERVICE,REG_SZ);
    result.matches=service.size()==1&&service[0]==expected&&!result.reboot;
    return result;
}
}
