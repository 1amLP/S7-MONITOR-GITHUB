#pragma once
// The endpoint is fixed by its signed PnP hardware identity, not a writable
// application command. This header is also exercised by host-only tests.
#include "../SelectionProtocol.h"
#include <cstddef>
#include <cstdint>
#include <string_view>
namespace s7camera::legacy {
inline constexpr std::wstring_view RearId=L"ROOT\\S7RearCamera10";
inline constexpr std::wstring_view FrontId=L"ROOT\\S7FrontCamera10";
inline constexpr wchar_t RearReference[]=L"S7Rear";
inline constexpr wchar_t FrontReference[]=L"S7Front";
inline constexpr wchar_t RearClass[]=L"{24E18666-E40B-4DC0-A151-64B642D8D7C9}";
inline constexpr wchar_t FrontClass[]=L"{D5C7CCFE-5E13-451A-8BDD-E47C2A978B01}";
inline constexpr uint32_t MinimumBuild=19041;
inline bool equalId(std::wstring_view a,std::wstring_view b) noexcept {
    if(a.size()!=b.size())return false;
    for(size_t i=0;i<a.size();++i){
        auto lower=[](wchar_t c){return c>=L'A'&&c<=L'Z'?wchar_t(c-L'A'+L'a'):c;};
        if(lower(a[i])!=lower(b[i]))return false;
    }
    return true;
}
// Consume the entire bounded MULTI_SZ. A second entry, malformed terminator,
// unknown identity or trailing nonzero data is rejected, not guessed.
inline bool sensorForIds(const wchar_t* raw,size_t chars,selection::Sensor& sensor) noexcept {
    if(!raw||chars<3||chars>256)return false;
    size_t length=0;while(length<chars&&raw[length])++length;
    if(length+1>=chars)return false;
    for(size_t i=length;i<chars;++i)if(raw[i])return false;
    const std::wstring_view id(raw,length);
    if(equalId(id,RearId)){sensor=selection::Sensor::Rear;return true;}
    if(equalId(id,FrontId)){sensor=selection::Sensor::Front;return true;}
    return false;
}
inline const wchar_t* reference(selection::Sensor sensor) noexcept {
    return sensor==selection::Sensor::Rear?RearReference:FrontReference;
}
inline std::vector<WebcamMode> declaredModes(selection::Sensor sensor) {
    selection::Message catalog;
    // Never publish diagnostic high-FPS entries, even when offline.
    for(auto m:selection::modes(sensor))if(webcamEligible(m))
        catalog.masks[uint8_t(sensor)]|=selection::bit(sensor,m);
    return selection::admitted(catalog,sensor);
}
}
