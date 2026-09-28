#pragma once
#include <cstdint>
#include <string_view>

namespace s7endpoint {
constexpr uint32_t Camera=2,Speaker=4,Microphone=8,Touch=16,Pad=32,SniperTouch=64;
constexpr bool validFlags(uint32_t flags){return !(flags&~127u)&&(flags&(Touch|Pad))!=(Touch|Pad);}
constexpr bool starts(std::wstring_view text,std::wstring_view prefix){return text.substr(0,prefix.size())==prefix;}
// Input is uppercase. Container/serial validation is additionally mandatory.
constexpr bool nativeFunction(std::wstring_view id){
    return starts(id,L"USB\\VID_04E8&PID_A7C1&MI_")||starts(id,L"HID\\VID_04E8&PID_A7C1&MI_");
}
constexpr bool matches(uint32_t bit,std::wstring_view id,std::wstring_view compatible){
    if(!nativeFunction(id))return false;
    if(bit==Camera)return starts(id,L"USB\\")&&compatible==L"USB\\CLASS_FF&SUBCLASS_53&PROT_72";
    if(!starts(id,L"HID\\"))return false;
    const bool sniper=starts(id,L"HID\\VID_04E8&PID_A7C1&MI_01&COL06\\");
    if(bit==Touch)return !sniper&&compatible==L"HID_DEVICE_UP:000D_U:0004";
    if(bit==SniperTouch)return sniper&&compatible==L"HID_DEVICE_UP:000D_U:0004";
    if(bit==Pad)return compatible==L"HID_DEVICE_UP:000D_U:0005";
    return false;
}
}
