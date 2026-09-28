#pragma once
#include "../monitor/SniperChannel.h"
#include "SniperMappingLock.h"
#include "DeviceIdentity.h"
#include "UsbIdentity.h"
#include "../DeviceConfig.h"
#include <setupapi.h>
#include <cfgmgr32.h>
#include <hidsdi.h>
#include <hidpi.h>
#include <initguid.h>
#include <ntddvdeo.h>
#include <cwctype>

namespace s7 {
inline std::wstring lowerSniperPath(std::wstring s){for(auto& c:s)c=static_cast<wchar_t>(towlower(c));return s;}
inline bool sniperTouchPath(const std::wstring& path){
    const auto p=lowerSniperPath(path);
    return p.starts_with(L"\\\\?\\hid#vid_04e8&pid_a7c1&mi_01&col06#")&&p.find(L"{4d1e55b2-f16f-11cf-88cb-001111000030}")!=std::wstring::npos;
}
inline bool sniperRawName(const wchar_t* path,UINT copied,UINT capacity){
    return path&&copied>1&&copied<=capacity&&path[copied-1]==L'\0'&&wcsnlen_s(path,copied)==copied-1&&sniperTouchPath(path);
}
struct SniperPointerDevice {HANDLE device=nullptr;std::wstring path;RECT display{};};
inline SniperPointerDevice sniperPointerDevice(){
    UINT32 count=0;wincheck(GetPointerDevices(&count,nullptr),"Query Sniper pointer devices");
    if(count>256)throw Failure(E_INVALIDARG,"Pointer device count outside bound");
    std::vector<POINTER_DEVICE_INFO> devices(count);
    if(count)wincheck(GetPointerDevices(&count,devices.data()),"Read Sniper pointer devices");
    SniperPointerDevice result;
    for(UINT32 i=0;i<count;++i){
        wchar_t path[512]{};UINT chars=UINT(std::size(path));
        const UINT copied=GetRawInputDeviceInfoW(devices[i].device,RIDI_DEVICENAME,path,&chars);
        if(copied==UINT(-1)||!sniperRawName(path,copied,UINT(std::size(path))))continue;
        if(result.device)throw Failure(HRESULT_FROM_WIN32(ERROR_DUP_NAME),"Duplicate Sniper touchscreen");
        RECT input{};wincheck(GetPointerDeviceRects(devices[i].device,&input,&result.display),"Sniper display association");
        result.device=devices[i].device;result.path=path;
    }
    return result;
}
inline DEVINST verifySniperMappingTarget(const std::wstring& touch,const std::wstring& display){
    if(!sniperTouchPath(touch)||!lowerSniperPath(display).starts_with(L"\\\\?\\display#"))throw Failure(E_ACCESSDENIED,"Unowned Sniper mapping request");
    GUID hid{};HidD_GetHidGuid(&hid);DEVINST foundTouch=0;bool foundDisplay=false;
    for(const auto& guid:{hid,GUID_DEVINTERFACE_MONITOR}){
        const auto set=SetupDiGetClassDevsW(&guid,nullptr,nullptr,DIGCF_PRESENT|DIGCF_DEVICEINTERFACE);
        if(set==INVALID_HANDLE_VALUE)throw Failure(HRESULT_FROM_WIN32(GetLastError()),"Enumerate mapping devices");
        struct Close{HDEVINFO set;~Close(){SetupDiDestroyDeviceInfoList(set);}}close{set};
        for(DWORD i=0;;++i){
            SP_DEVICE_INTERFACE_DATA item{sizeof(item)};
            if(!SetupDiEnumDeviceInterfaces(set,nullptr,&guid,i,&item)){if(GetLastError()==ERROR_NO_MORE_ITEMS)break;throw Failure(HRESULT_FROM_WIN32(GetLastError()),"Mapping device enumeration");}
            DWORD n=0;SetupDiGetDeviceInterfaceDetailW(set,&item,nullptr,0,&n,nullptr);
            if(n<sizeof(SP_DEVICE_INTERFACE_DETAIL_DATA_W)||n>65536)throw Failure(E_INVALIDARG,"Mapping device path extent");
            Bytes bytes(n);auto* detail=reinterpret_cast<SP_DEVICE_INTERFACE_DETAIL_DATA_W*>(bytes.data());detail->cbSize=sizeof(*detail);
            SP_DEVINFO_DATA node{sizeof(node)};wincheck(SetupDiGetDeviceInterfaceDetailW(set,&item,detail,n,nullptr,&node),"Mapping device path");
            const auto path=lowerSniperPath(detail->DevicePath);
            if(IsEqualGUID(guid,GUID_DEVINTERFACE_MONITOR)){if(path==lowerSniperPath(display))foundDisplay=true;continue;}
            if(path!=lowerSniperPath(touch))continue;
            Handle file(CreateFileW(detail->DevicePath,0,FILE_SHARE_READ|FILE_SHARE_WRITE,nullptr,OPEN_EXISTING,0,nullptr));wincheck(bool(file),"Open mapped Sniper touchscreen");
            wchar_t serial[128]{};wincheck(HidD_GetSerialNumberString(file.get(),serial,sizeof(serial)),"Sniper touchscreen serial");
            if(std::wstring(serial)!=TargetSerial)throw Failure(E_ACCESSDENIED,"Foreign Sniper mapping device");
            PHIDP_PREPARSED_DATA parsed=nullptr;wincheck(HidD_GetPreparsedData(file.get(),&parsed),"Sniper HID capabilities");
            struct Free{PHIDP_PREPARSED_DATA p;~Free(){HidD_FreePreparsedData(p);}}free{parsed};
            HIDP_CAPS caps{};
            if(HidP_GetCaps(parsed,&caps)!=HIDP_STATUS_SUCCESS||caps.UsagePage!=0x0d||caps.Usage!=4||caps.InputReportByteLength!=34)throw Failure(E_ACCESSDENIED,"Unexpected Sniper touchscreen collection");
            foundTouch=node.DevInst;
        }
    }
    if(!foundTouch||!foundDisplay)throw Failure(HRESULT_FROM_WIN32(ERROR_NOT_FOUND),"Sniper mapping devices are not both present");
    return foundTouch;
}
inline void writeSniperMapping(const std::wstring& touch,const std::wstring& display){
    std::lock_guard<std::mutex> guard(sniperMappingMutex());
    const DEVINST node=verifySniperMappingTarget(touch,display);
    const auto name=L"20-"+touch;
    struct Key{HKEY h=nullptr;~Key(){if(h)RegCloseKey(h);}} key,backup;
    auto reg=[](LSTATUS result,const char* where){if(result!=ERROR_SUCCESS)throw Failure(HRESULT_FROM_WIN32(result),where);};
    reg(RegCreateKeyExW(HKEY_LOCAL_MACHINE,L"SOFTWARE\\Microsoft\\Wisp\\Pen\\Digimon",0,nullptr,0,KEY_QUERY_VALUE|KEY_SET_VALUE,nullptr,&key.h,nullptr),"Open owned digitizer mapping value");
    DWORD type=0,n=0;auto status=RegQueryValueExW(key.h,name.c_str(),nullptr,&type,nullptr,&n);
    if(status!=ERROR_SUCCESS&&status!=ERROR_FILE_NOT_FOUND)reg(status,"Read previous digitizer mapping");
    if(n>65536)throw Failure(E_INVALIDARG,"Previous digitizer mapping is oversized");
    Bytes previous(n);const bool existed=status==ERROR_SUCCESS;
    if(existed)reg(RegQueryValueExW(key.h,name.c_str(),nullptr,&type,previous.data(),&n),"Read previous mapping bytes");
    const DWORD wantedBytes=DWORD((display.size()+1)*sizeof(wchar_t));
    const bool same=existed&&type==REG_SZ&&n==wantedBytes&&!std::memcmp(previous.data(),display.c_str(),wantedBytes);
    if(!same){
        uint64_t hash=14695981039346656037ull;for(const auto c:lowerSniperPath(touch)){hash^=uint16_t(c);hash*=1099511628211ull;}
        wchar_t suffix[32]{};swprintf_s(suffix,L"%016llx",static_cast<unsigned long long>(hash));
        const auto backupPath=std::wstring(L"SOFTWARE\\S7 Appliance\\SniperMapping\\")+suffix;
        DWORD created=0;reg(RegCreateKeyExW(HKEY_LOCAL_MACHINE,backupPath.c_str(),0,nullptr,0,KEY_QUERY_VALUE|KEY_SET_VALUE,nullptr,&backup.h,&created),"Open Sniper mapping backup");
        if(created==REG_CREATED_NEW_KEY){
            const DWORD exists=existed?1:0;
            reg(RegSetValueExW(backup.h,L"Device",0,REG_SZ,reinterpret_cast<const BYTE*>(touch.c_str()),DWORD((touch.size()+1)*sizeof(wchar_t))),"Back up mapping identity");
            reg(RegSetValueExW(backup.h,L"Existed",0,REG_DWORD,reinterpret_cast<const BYTE*>(&exists),sizeof(exists)),"Back up mapping existence");
            reg(RegSetValueExW(backup.h,L"Type",0,REG_DWORD,reinterpret_cast<const BYTE*>(&type),sizeof(type)),"Back up mapping type");
            reg(RegSetValueExW(backup.h,L"Data",0,REG_BINARY,previous.data(),DWORD(previous.size())),"Back up mapping data");
        }else{
            wchar_t original[512]{};DWORD size=sizeof(original),kind=0;
            reg(RegQueryValueExW(backup.h,L"Device",nullptr,&kind,reinterpret_cast<BYTE*>(original),&size),"Verify mapping backup identity");
            if(kind!=REG_SZ||size>sizeof(original)||!size||original[std::size(original)-1]||lowerSniperPath(original)!=lowerSniperPath(touch))throw Failure(E_ACCESSDENIED,"Mapping backup belongs to another device");
        }
        reg(RegSetValueExW(key.h,name.c_str(),0,REG_SZ,reinterpret_cast<const BYTE*>(display.c_str()),wantedBytes),"Map only Sniper HID to primary display");
        wchar_t actual[128]{};DWORD size=sizeof(actual),kind=0;
        reg(RegQueryValueExW(key.h,name.c_str(),nullptr,&kind,reinterpret_cast<BYTE*>(actual),&size),"Read back Sniper display mapping");
        if(kind!=REG_SZ||size!=wantedBytes||std::memcmp(actual,display.c_str(),wantedBytes))throw Failure(E_FAIL,"Sniper mapping readback differs");
    }
    auto cm=[](CONFIGRET result,const char* where){if(result!=CR_SUCCESS)throw Failure(HRESULT_FROM_WIN32(CM_MapCrToWin32Err(result,ERROR_GEN_FAILURE)),where);};
    cm(CM_Disable_DevNode(node,CM_DISABLE_UI_NOT_OK),"Refresh only Sniper HID association");
    ULONG flags=0,problem=0;
    cm(CM_Get_DevNode_Status(&flags,&problem,node,0),"Check Sniper HID disable");
    if(!(flags&DN_HAS_PROBLEM)||problem!=CM_PROB_DISABLED)throw Failure(E_FAIL,"Sniper HID did not disable");
    cm(CM_Enable_DevNode(node,0),"Restore Sniper HID after association update");
    cm(CM_Get_DevNode_Status(&flags,&problem,node,0),"Check Sniper HID restore");
    if(!(flags&DN_STARTED)||(flags&DN_HAS_PROBLEM))throw Failure(E_FAIL,"Sniper HID did not restart");
}
}
