#pragma once
#include <windows.h>
#include <cwchar>

namespace s7camera {
// Lifecycle/error records only. No images, per-frame writes or capture polling.
inline void cameraTrace(char const* stage,unsigned sensor,HRESULT result,HRESULT cleanup=S_OK)noexcept{
    wchar_t text[512]{};
    std::swprintf(text,512,L"pid=%lu sensor=%u stage=%hs result=0x%08lx cleanup=0x%08lx",
        GetCurrentProcessId(),sensor,stage,static_cast<unsigned long>(result),static_cast<unsigned long>(cleanup));
    OutputDebugStringW(text);
    HANDLE log=RegisterEventSourceW(nullptr,L"S7Camera");
    if(log){LPCWSTR items[]{text};ReportEventW(log,FAILED(result)?EVENTLOG_ERROR_TYPE:EVENTLOG_INFORMATION_TYPE,0,1,nullptr,1,0,items,nullptr);DeregisterEventSource(log);}
}
}
