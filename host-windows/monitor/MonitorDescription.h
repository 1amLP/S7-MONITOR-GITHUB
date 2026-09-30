// Selected native monitor mode. No desktop-wide DPI/registry writes.
#pragma once
#include <array>
#include <cstddef>
#include <cstdint>
#include <string_view>

namespace s7 {
// Windows samples the virtual desktop faster than the 60 FPS USB stream.
constexpr uint32_t MonitorCaptureFPS=120;
struct MonitorTiming {
    uint32_t width=1280, height=720, totalWidth=1650, totalHeight=750;
    uint32_t pixelClock=74250000, fps=60;
};
inline bool monitorTiming(uint32_t fps, MonitorTiming& t,uint32_t width=1280,uint32_t height=720) noexcept {
    if((fps!=60&&fps!=MonitorCaptureFPS) || !((width==1280&&height==720)||(width==2560&&height==1440))) return false;
    t=MonitorTiming{};
	if(width==2560)t={2560,1440,2720,1500,244800000,60};
    t.pixelClock*=fps/60;t.fps=fps;
    return true;
}
using MonitorEdid=std::array<uint8_t,128>;
constexpr uint16_t MonitorWidthMM=113, MonitorHeightMM=64;
inline uint32_t monitorSerial(std::wstring_view serial) noexcept {
    uint32_t h=2166136261u;
    for(wchar_t c:serial) { h^=uint32_t(c)&255u; h*=16777619u; }
    return h?h:1u;
}
inline MonitorEdid monitorEdid(uint32_t fps, uint32_t serial,uint32_t width=1280,uint32_t height=720) noexcept {
    MonitorTiming t;
    MonitorEdid e{};
    if(!monitorTiming(fps,t,width,height) || !serial) return e;
    const uint8_t header[]={0,255,255,255,255,255,255,0};
    for(size_t i=0;i<8;i++)e[i]=header[i];
    // "SNP" is the virtual S7 Native Project identity, not a vendor certification.
    constexpr uint16_t m=(19<<10)|(14<<5)|16;
    e[8]=uint8_t(m>>8);e[9]=uint8_t(m&0xffu);
    e[10]=0x30;e[11]=0x09; // stable product, independent of refresh rate
    for(unsigned i=0;i<4;i++)e[12+i]=uint8_t(serial>>(8*i));
    e[16]=1;e[17]=36; // fixed build identity: 2026, not a varying build timestamp
    e[18]=1;e[19]=4;
    e[20]=0x80; // digital, unspecified link type/depth; no fake HDMI/audio
    e[21]=uint8_t(MonitorWidthMM/10);e[22]=uint8_t(MonitorHeightMM/10);
    e[23]=120; // gamma 2.20
    e[24]=0x06; // sRGB, first DTD preferred; no standby/suspend/active-off claim
    // sRGB primaries and white point encoded in EDID 10-bit units.
    e[25]=0xee;e[26]=0x91;e[27]=0xa3;e[28]=0x54;e[29]=0x4c;
    e[30]=0x99;e[31]=0x26;e[32]=0x0f;e[33]=0x50;e[34]=0x54;
    // No legacy or low-resolution modes: every standard timing is unused.
    for(size_t i=38;i<54;i++)e[i]=1;
    uint8_t* d=e.data()+54;
    const uint32_t hblank=t.totalWidth-t.width, vblank=t.totalHeight-t.height;
    const uint32_t clock=t.pixelClock/10000;
    d[0]=uint8_t(clock);d[1]=uint8_t(clock>>8);
    d[2]=uint8_t(t.width);d[3]=uint8_t(hblank);d[4]=uint8_t((t.width>>8)<<4|(hblank>>8));
    d[5]=uint8_t(t.height);d[6]=uint8_t(vblank);d[7]=uint8_t((t.height>>8)<<4|(vblank>>8));
	// Exact virtual clocks with positive separate sync; not a panel refresh claim.
    d[8]=uint8_t(width==1280?110:48);d[9]=uint8_t(width==1280?40:32);d[10]=(5<<4)|5;d[11]=0;
    d[12]=uint8_t(MonitorWidthMM);d[13]=uint8_t(MonitorHeightMM);
    d[14]=uint8_t((MonitorWidthMM>>8)<<4|(MonitorHeightMM>>8));
    d[17]=0x1e; // progressive, digital separate sync, positive H/V
    auto text=[&](size_t off,uint8_t tag,const char* s) {
        e[off+3]=tag;
        for(size_t i=0;i<13;i++)e[off+5+i]=' ';
        size_t i=0;for(;s[i] && i<12;i++)e[off+5+i]=uint8_t(s[i]);
        e[off+5+i]='\n';
    };
    text(72,0xfc,"S7 Monitor");
    char digits[9]{};
    constexpr char hex[]="0123456789ABCDEF";
    for(unsigned i=0;i<8;i++)digits[i]=hex[(serial>>(28-4*i))&15];
    text(90,0xff,digits);
    // Valid unused descriptor; no misleading range formula for extra modes.
    e[111]=0x10;
    unsigned sum=0;for(size_t i=0;i<127;i++)sum+=e[i];
    e[127]=uint8_t((256u-(sum&255u))&255u);
    return e;
}
inline bool parseMonitorEdid(const void* data,size_t n,MonitorTiming& result) noexcept {
    if(!data || n!=128) return false;
    auto p=static_cast<const uint8_t*>(data);
    unsigned sum=0;for(size_t i=0;i<n;i++)sum+=p[i];
    if(sum&255) return false;
    const uint32_t serial=uint32_t(p[12])|uint32_t(p[13])<<8|uint32_t(p[14])<<16|uint32_t(p[15])<<24;
    if(!serial) return false;
    for(const uint32_t fps:{60u,MonitorCaptureFPS})for(const uint32_t width:{1280u,2560u}){
        const uint32_t height=width*9/16;
        const auto expected=monitorEdid(fps,serial,width,height);
        bool match=true;for(size_t i=0;i<expected.size();i++) if(p[i]!=expected[i]){match=false;break;}
        if(match) return monitorTiming(fps,result,width,height);
    }
    return false; // never guess a timing from an unowned EDID blob
}
} // namespace s7
