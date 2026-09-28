#pragma once
#define NOMINMAX
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mferror.h>
#include <mfreadwrite.h>
#ifndef S7_LEGACY_FRAME_SERVER
#include <mfvirtualcamera.h>
#endif
#include <wrl.h>
#include <ks.h>
#include <ksmedia.h>
#include <ksproxy.h>
#include <string>
#include <cstring>
#include "SelectionProtocol.h"
#include <new>
namespace s7camera {
using Microsoft::WRL::ComPtr;
using Microsoft::WRL::Make;
template<class... T> using ComObject=Microsoft::WRL::RuntimeClass<Microsoft::WRL::RuntimeClassFlags<Microsoft::WRL::ClassicCom>,T...>;
#ifndef S7_LEGACY_FRAME_SERVER
inline constexpr GUID ClassId={0x4c850e86,0x1698,0x44bf,{0x89,0x3e,0x43,0xec,0xf6,0x1b,0xaa,0xa9}};
inline constexpr wchar_t ClassText[]=L"{4C850E86-1698-44BF-893E-43ECF61BAAA9}";
inline constexpr wchar_t LegacyName[]=L"S7 H.264 Camera";
inline constexpr wchar_t Name[]=L"S7 Rear Camera";
inline constexpr GUID FrontClassId={0x14986cdd,0x1da2,0x4a72,{0xb9,0x53,0x91,0x4e,0x65,0x93,0x49,0x21}};
inline constexpr wchar_t FrontClassText[]=L"{14986CDD-1DA2-4A72-B953-914E65934921}";
inline constexpr wchar_t FrontName[]=L"S7 Front Camera";
#else
// Separate COM identities: installation never overwrites the Windows 11 source.
inline constexpr GUID ClassId={0x24e18666,0xe40b,0x4dc0,{0xa1,0x51,0x64,0xb6,0x42,0xd8,0xd7,0xc9}};
inline constexpr wchar_t ClassText[]=L"{24E18666-E40B-4DC0-A151-64B642D8D7C9}";
inline constexpr GUID FrontClassId={0xd5c7ccfe,0x5e13,0x451a,{0x8b,0xdd,0xe4,0x7c,0x2a,0x97,0x8b,0x01}};
inline constexpr wchar_t FrontClassText[]=L"{D5C7CCFE-5E13-451A-8BDD-E47C2A978B01}";
inline constexpr wchar_t Name[]=L"S7 Rear Camera";
inline constexpr wchar_t FrontName[]=L"S7 Front Camera";
#endif
inline GUID const& endpointClass(selection::Sensor sensor){return sensor==selection::Sensor::Rear?ClassId:FrontClassId;}
inline wchar_t const* endpointClassText(selection::Sensor sensor){return sensor==selection::Sensor::Rear?ClassText:FrontClassText;}
inline wchar_t const* endpointName(selection::Sensor sensor){return sensor==selection::Sensor::Rear?Name:FrontName;}

inline void check(HRESULT hr){if(FAILED(hr))throw hr;}
template<class F> HRESULT guarded(F&& f) noexcept {try{f();return S_OK;}catch(HRESULT hr){return hr;}catch(std::bad_alloc const&){return E_OUTOFMEMORY;}catch(...){return E_FAIL;}}
inline HRESULT missing(ULONG* bytes){if(bytes)*bytes=0;return HRESULT_FROM_WIN32(ERROR_SET_NOT_FOUND);}
}
