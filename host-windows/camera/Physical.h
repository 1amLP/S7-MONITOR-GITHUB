#pragma once
#include "Camera.h"
#include "Preference.h"
#include "DeviceIdentity.h"
#include "UsbIdentity.h"
#include <algorithm>
#include <cwctype>

namespace s7camera {
inline ComPtr<IMFActivate> physicalActivation(){
    Preference identity;
    GUID const expected=identity.containerId();
    ComPtr<IMFAttributes> filter;check(MFCreateAttributes(&filter,1));
    check(filter->SetGUID(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE,MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID));
    IMFActivate** devices=nullptr;UINT32 count=0;check(MFEnumDeviceSources(filter.Get(),&devices,&count));
    struct List {IMFActivate** p;UINT32 n;~List(){for(UINT32 i=0;i<n;++i)p[i]->Release();CoTaskMemFree(p);}}cleanup{devices,count};
    ComPtr<IMFActivate> chosen;
    for(UINT32 i=0;i<count;++i){
        wchar_t* raw=nullptr;UINT32 size=0;
        if(FAILED(devices[i]->GetAllocatedString(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_SYMBOLIC_LINK,&raw,&size)))continue;
        std::wstring path(raw,size);CoTaskMemFree(raw);std::transform(path.begin(),path.end(),path.begin(),towlower);
        if(!s7UsbFunction(path))continue;
        GUID const actual=containerForInterface(path);
        if(!IsEqualGUID(actual,expected))continue;
        if(chosen)throw HRESULT_FROM_WIN32(ERROR_DUP_NAME);chosen=devices[i];
    }
    if(!chosen)throw HRESULT_FROM_WIN32(ERROR_DEVICE_NOT_CONNECTED);
    return chosen;
}
inline std::wstring physicalLink(){
    auto activation=physicalActivation();wchar_t* raw=nullptr;UINT32 size=0;
    check(activation->GetAllocatedString(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_SYMBOLIC_LINK,&raw,&size));
    std::wstring result(raw,size);CoTaskMemFree(raw);return result;
}
inline ComPtr<IMFMediaSource> physicalCamera(){
    auto activation=physicalActivation();ComPtr<IMFMediaSource> source;
    check(activation->ActivateObject(IID_PPV_ARGS(&source)));return source;
}
}
