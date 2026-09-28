#pragma once
#include "Camera.h"
#include "ModePolicy.h"
#include "FeatureRead.h"
#include "DeviceIdentity.h"
#include "UsbIdentity.h"
#include "../DeviceConfig.h"
#include <setupapi.h>
#include <hidsdi.h>
#include <hidpi.h>
#include <vector>
#include <algorithm>
#include <cwctype>
namespace s7camera {
class Preference {
    HANDLE file=INVALID_HANDLE_VALUE;
    GUID container{};
public:
    explicit Preference(USHORT usage=1,USHORT reportBytes=17){
        GUID guid{};HidD_GetHidGuid(&guid);
        auto set=SetupDiGetClassDevsW(&guid,nullptr,nullptr,DIGCF_PRESENT|DIGCF_DEVICEINTERFACE);
        if(set==INVALID_HANDLE_VALUE)throw HRESULT_FROM_WIN32(GetLastError());
        struct Guard{HDEVINFO set;~Guard(){SetupDiDestroyDeviceInfoList(set);}}guard{set};
        try{for(DWORD i=0;;++i){
            SP_DEVICE_INTERFACE_DATA item{sizeof(item)};
            if(!SetupDiEnumDeviceInterfaces(set,nullptr,&guid,i,&item)){if(GetLastError()==ERROR_NO_MORE_ITEMS)break;throw HRESULT_FROM_WIN32(GetLastError());}
            DWORD size=0;SetupDiGetDeviceInterfaceDetailW(set,&item,nullptr,0,&size,nullptr);
            if(size<sizeof(SP_DEVICE_INTERFACE_DETAIL_DATA_W)||size>65536)throw E_FAIL;
            std::vector<BYTE> data(size);auto detail=reinterpret_cast<SP_DEVICE_INTERFACE_DETAIL_DATA_W*>(data.data());detail->cbSize=sizeof(*detail);
            SP_DEVINFO_DATA device{sizeof(device)};
            if(!SetupDiGetDeviceInterfaceDetailW(set,&item,detail,size,nullptr,&device))throw HRESULT_FROM_WIN32(GetLastError());
            std::wstring path=detail->DevicePath;std::transform(path.begin(),path.end(),path.begin(),towlower);
            if(!s7UsbFunction(path))continue;
            auto handle=CreateFileW(path.c_str(),0,FILE_SHARE_READ|FILE_SHARE_WRITE,nullptr,OPEN_EXISTING,FILE_FLAG_OVERLAPPED,nullptr);
            if(handle==INVALID_HANDLE_VALUE)continue;
            PHIDP_PREPARSED_DATA parsed=nullptr;HIDP_CAPS caps{};
            bool match=HidD_GetPreparsedData(handle,&parsed)&&HidP_GetCaps(parsed,&caps)==HIDP_STATUS_SUCCESS&&caps.UsagePage==0xff53&&caps.Usage==usage&&caps.FeatureReportByteLength==reportBytes;
            if(parsed)HidD_FreePreparsedData(parsed);
            if(!match){CloseHandle(handle);continue;}
            wchar_t serial[128]{};
            if(!HidD_GetSerialNumberString(handle,serial,sizeof(serial))||std::wstring(serial)!=s7::TargetSerial){CloseHandle(handle);throw E_ACCESSDENIED;}
            if(file!=INVALID_HANDLE_VALUE){CloseHandle(handle);throw HRESULT_FROM_WIN32(ERROR_DUP_NAME);}
            file=handle;
            container=containerForDevice(set,device);
        }
        if(file==INVALID_HANDLE_VALUE)throw HRESULT_FROM_WIN32(ERROR_DEVICE_NOT_CONNECTED);
        }catch(...){if(file!=INVALID_HANDLE_VALUE)CloseHandle(file);file=INVALID_HANDLE_VALUE;throw;}
    }
    GUID const& containerId()const noexcept{return container;}
 HANDLE handle()const noexcept{return file;}
    ~Preference(){if(file!=INVALID_HANDLE_VALUE)CloseHandle(file);}
    Preference(Preference const&)=delete;
    WebcamMode read(HANDLE cancel=nullptr){
        auto data=readFeature(file,cancel);
        if(data[0]!=7||std::memcmp(data.data()+1,"S7W1",4))throw E_INVALIDARG;
        WebcamMode m{};std::memcpy(&m.width,data.data()+5,4);std::memcpy(&m.height,data.data()+9,4);std::memcpy(&m.fps,data.data()+13,4);
        if(!knownMode(m))throw E_INVALIDARG;
        return m;
    }
};
inline WebcamMode preference(){return Preference().read();}
}
