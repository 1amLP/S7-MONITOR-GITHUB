#pragma once
#include "Camera.h"

namespace s7camera {
// Windows exposes no documented per-endpoint enable API. Isolate the existing
// PolicyConfig ABI used by Sound Control Panel; fail rather than touch registry
// state or disable the shared USB audio adapter when the interface is absent.
struct __declspec(uuid("F8679F50-850A-41CF-9C72-430F290290C8")) AudioPolicy : IUnknown {
    virtual HRESULT STDMETHODCALLTYPE Unused1()=0;
    virtual HRESULT STDMETHODCALLTYPE Unused2()=0;
    virtual HRESULT STDMETHODCALLTYPE Unused3()=0;
    virtual HRESULT STDMETHODCALLTYPE Unused4()=0;
    virtual HRESULT STDMETHODCALLTYPE Unused5()=0;
    virtual HRESULT STDMETHODCALLTYPE Unused6()=0;
    virtual HRESULT STDMETHODCALLTYPE Unused7()=0;
    virtual HRESULT STDMETHODCALLTYPE Unused8()=0;
    virtual HRESULT STDMETHODCALLTYPE Unused9()=0;
    virtual HRESULT STDMETHODCALLTYPE Unused10()=0;
    virtual HRESULT STDMETHODCALLTYPE Unused11()=0;
    virtual HRESULT STDMETHODCALLTYPE SetEndpointVisibility(LPCWSTR endpoint,INT visible)=0;
};
inline void setAudioVisibility(LPCWSTR endpoint,bool enabled){
    constexpr CLSID policyClass{0x870af99c,0x171d,0x4f9e,{0xaf,0x0d,0xe6,0x3d,0xf4,0x0c,0x2b,0xc9}};
    ComPtr<AudioPolicy> policy;
    check(CoCreateInstance(policyClass,nullptr,CLSCTX_INPROC_SERVER,IID_PPV_ARGS(&policy)));
    check(policy->SetEndpointVisibility(endpoint,enabled?1:0));
}
}
