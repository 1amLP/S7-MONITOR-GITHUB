#include "DeviceIdentity.h"
#include <initguid.h>
#include <devpkey.h>
#include <vector>

namespace s7camera {
GUID containerForDevice(HDEVINFO set,SP_DEVINFO_DATA& device){
    GUID id{};DEVPROPTYPE type=0;DWORD size=0;
    if(!SetupDiGetDevicePropertyW(set,&device,&DEVPKEY_Device_ContainerId,&type,
        reinterpret_cast<BYTE*>(&id),sizeof(id),&size,0))throw HRESULT_FROM_WIN32(GetLastError());
    if(type!=DEVPROP_TYPE_GUID||size!=sizeof(id)||IsEqualGUID(id,GUID_NULL))throw E_ACCESSDENIED;
    return id;
}
GUID containerForInterface(std::wstring const& path){
    auto set=SetupDiCreateDeviceInfoList(nullptr,nullptr);
    if(set==INVALID_HANDLE_VALUE)throw HRESULT_FROM_WIN32(GetLastError());
    struct Guard{HDEVINFO set;~Guard(){SetupDiDestroyDeviceInfoList(set);}}guard{set};
    SP_DEVICE_INTERFACE_DATA item{sizeof(item)};
    if(!SetupDiOpenDeviceInterfaceW(set,path.c_str(),0,&item))throw HRESULT_FROM_WIN32(GetLastError());
    DWORD size=0;
    SetupDiGetDeviceInterfaceDetailW(set,&item,nullptr,0,&size,nullptr);
    if(size<sizeof(SP_DEVICE_INTERFACE_DETAIL_DATA_W)||size>65536)throw E_FAIL;
    std::vector<BYTE> storage(size);
    auto detail=reinterpret_cast<SP_DEVICE_INTERFACE_DETAIL_DATA_W*>(storage.data());
    detail->cbSize=sizeof(*detail);SP_DEVINFO_DATA device{sizeof(device)};
    if(!SetupDiGetDeviceInterfaceDetailW(set,&item,detail,size,nullptr,&device))throw HRESULT_FROM_WIN32(GetLastError());
    return containerForDevice(set,device);
}
}
