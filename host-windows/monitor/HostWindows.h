#pragma once
#ifndef NOMINMAX
#define NOMINMAX
#endif
#ifdef UMDF_DRIVER
#define WIN32_NO_STATUS
#endif
#include <windows.h>
#ifdef UMDF_DRIVER
#undef WIN32_NO_STATUS
#include <ntstatus.h>
#endif
#include <wrl/client.h>
#include <string>
#include <stdexcept>
#include <memory>
#include <utility>
#include <cstdint>
#include <cstdio>
#include <algorithm>
namespace s7 {
using Microsoft::WRL::ComPtr;
struct Failure:std::runtime_error {HRESULT code;Failure(HRESULT value,const char* where):std::runtime_error(where),code(value){}};
inline void check(HRESULT hr,const char* where){if(FAILED(hr))throw Failure(hr,where);}
inline void wincheck(BOOL ok,const char* where){if(!ok)throw Failure(HRESULT_FROM_WIN32(GetLastError()),where);}
inline void log(const char* what,HRESULT hr=S_OK){char p[640];sprintf_s(p,"S7Monitor: %s (0x%08lx)\n",what,(unsigned long)hr);OutputDebugStringA(p);}
class Handle {
    HANDLE h_=nullptr;
public:
    Handle()=default;explicit Handle(HANDLE h):h_(h){}
    Handle(const Handle&)=delete;Handle& operator=(const Handle&)=delete;
    Handle(Handle&& x)noexcept:h_(x.h_){x.h_=nullptr;}
    Handle& operator=(Handle&& x)noexcept{if(this!=&x){reset();h_=x.h_;x.h_=nullptr;}return *this;}
    ~Handle(){reset();}
    void reset(HANDLE h=nullptr){if(h_&&h_!=INVALID_HANDLE_VALUE)CloseHandle(h_);h_=h;}
    HANDLE get()const{return h_;}
    explicit operator bool()const{return h_&&h_!=INVALID_HANDLE_VALUE;}
};
struct ComRuntime {HRESULT init=CoInitializeEx(nullptr,COINIT_MULTITHREADED);ComRuntime(){check(init,"CoInitializeEx");}~ComRuntime(){if(SUCCEEDED(init))CoUninitialize();}};
inline uint64_t microseconds(){LARGE_INTEGER n,f;QueryPerformanceCounter(&n);QueryPerformanceFrequency(&f);return uint64_t(n.QuadPart/f.QuadPart)*1000000+uint64_t(n.QuadPart%f.QuadPart)*1000000/uint64_t(f.QuadPart);}
}
