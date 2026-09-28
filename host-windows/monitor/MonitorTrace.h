#pragma once
#include "HostWindows.h"

namespace s7 {
// Lifecycle/failure records only, never one record per frame or image content.
inline void monitorEvent(const char* text,HRESULT hr=S_OK)noexcept{
    log(text,hr);
    char message[1024]{};
    _snprintf_s(message,sizeof(message),_TRUNCATE,"%s (0x%08lx)",text,static_cast<unsigned long>(hr));
    HANDLE source=RegisterEventSourceA(nullptr,"S7Monitor");
    if(source){LPCSTR items[]{message};ReportEventA(source,FAILED(hr)?EVENTLOG_ERROR_TYPE:EVENTLOG_INFORMATION_TYPE,0,1,nullptr,1,0,items,nullptr);DeregisterEventSource(source);}
}
}
