#include "VirtualRegistration.h"
#include <iostream>
using namespace s7camera;
bool registeredName(wchar_t const* expected){
    ComPtr<IMFAttributes> filter;check(MFCreateAttributes(&filter,1));
    check(filter->SetGUID(MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE,MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID));
    IMFActivate** items=nullptr;UINT32 count=0;check(MFEnumDeviceSources(filter.Get(),&items,&count));
    struct List{IMFActivate** items;UINT32 count;~List(){for(UINT32 i=0;i<count;++i)items[i]->Release();CoTaskMemFree(items);}} list{items,count};
    const std::wstring plain(expected),visible=plain+L" (Windows Virtual Camera)";
    for(UINT32 i=0;i<count;++i){
        wchar_t* value=nullptr;UINT32 length=0;
        if(FAILED(items[i]->GetAllocatedString(MF_DEVSOURCE_ATTRIBUTE_FRIENDLY_NAME,&value,&length)))continue;
        const std::wstring name(value);CoTaskMemFree(value);
        if(name==plain||name==visible)return true;
    }
    return false;
}
HRESULT removeCamera(IMFVirtualCamera* camera,wchar_t const* name){
    const HRESULT result=camera->Remove();
    if(result==MF_E_INVALIDREQUEST&&!registeredName(name)){
        std::wcout<<name<<L": no registered endpoint remains"<<std::endl;return S_OK;
    }
    return result;
}
