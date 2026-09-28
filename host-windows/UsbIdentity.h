#pragma once
#include <cstdint>
#include <string>
#include <string_view>

namespace s7identity {
// Separate native interface numbering from the legacy Android ADB composite.
// These are local experimental identities, not a vendor PID allocation.
constexpr bool product(uint16_t vendor, uint16_t id) {
    return vendor == 0x04e8 && (id == 0xa7c0 || id == 0xa7c1);
}
inline bool function(std::wstring_view input) {
    std::wstring s(input);
    for (auto& c : s) if (c >= L'A' && c <= L'Z') c = static_cast<wchar_t>(c - L'A' + L'a');
    constexpr std::wstring_view usb = L"\\\\?\\usb#", hid = L"\\\\?\\hid#";
    if (s.compare(0, usb.size(), usb) != 0 && s.compare(0, hid.size(), hid) != 0) return false;
    for (auto id : {std::wstring_view(L"vid_04e8&pid_a7c0"), std::wstring_view(L"vid_04e8&pid_a7c1")}) {
        const size_t end = usb.size() + id.size();
        if (s.size() > end && s.compare(usb.size(), id.size(), id) == 0 && (s[end] == L'&' || s[end] == L'#')) return true;
    }
    return false;
}
}
