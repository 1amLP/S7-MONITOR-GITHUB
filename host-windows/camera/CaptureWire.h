#pragma once
#include "ModePolicy.h"
#include <array>
#include <cstddef>
#include <cstdint>
#include <limits>
#include <span>
#include <stdexcept>

namespace s7camera::wire {
constexpr size_t HeaderBytes=64,MaxFrame=4u<<20;
constexpr uint16_t Open=1,Stop=2,Frame=1,Ack=2,Failure=3;
constexpr uint32_t KeyFrame=1,Discontinuity=2;
constexpr uint16_t u16(uint8_t const* p){return uint16_t(p[0])|uint16_t(uint16_t(p[1])<<8);}
constexpr uint32_t u32(uint8_t const* p){return uint32_t(p[0])|(uint32_t(p[1])<<8)|(uint32_t(p[2])<<16)|(uint32_t(p[3])<<24);}
constexpr uint64_t u64(uint8_t const* p){return uint64_t(u32(p))|(uint64_t(u32(p+4))<<32);}
inline void put16(uint8_t* p,uint16_t v){p[0]=uint8_t(v);p[1]=uint8_t(v>>8);}
inline void put32(uint8_t* p,uint32_t v){for(unsigned i=0;i<4;++i)p[i]=uint8_t(v>>(8*i));}
inline void put64(uint8_t* p,uint64_t v){put32(p,uint32_t(v));put32(p+4,uint32_t(v>>32));}
inline constexpr auto crcTable=[] {
    std::array<uint32_t,256> t{};
    for(uint32_t i=0;i<256;++i){uint32_t c=i;for(unsigned j=0;j<8;++j)c=(c>>1)^((c&1)?0xedb88320u:0u);t[i]=c;}
    return t;
}();
inline uint32_t crc(std::span<uint8_t const> b){uint32_t c=~0u;for(auto v:b)c=crcTable[(c^v)&255]^(c>>8);return ~c;}
struct Header {
    uint16_t kind=0;
    uint64_t session=0,sequence=0,pts=0;
    WebcamMode mode{};
    uint32_t flags=0,bytes=0,checksum=0,error=0;
    size_t packetBytes()const {size_t n=HeaderBytes+bytes;return n+(n%512==0?1:0);}
};
inline bool parse(std::span<uint8_t const> p,Header& h){
    if(p.size()!=HeaderBytes||p[0]!='S'||p[1]!='7'||p[2]!='W'||p[3]!='F'||u16(p.data()+4)!=1||u32(p.data()+60)!=crc(p.first(60)))return false;
    auto b=p.data();h.kind=u16(b+6);h.session=u64(b+8);h.sequence=u64(b+16);h.pts=u64(b+24);
    h.mode={u32(b+32),u32(b+36),u32(b+40)};h.flags=u32(b+44);h.bytes=u32(b+48);h.checksum=u32(b+52);h.error=u32(b+56);
    if(!h.session||h.pts>uint64_t(std::numeric_limits<int64_t>::max()/10)||(h.flags&~(KeyFrame|Discontinuity)))return false;
    if(h.kind==Frame)return webcamEligible(h.mode)&&h.sequence&&h.bytes>=4&&h.bytes<=MaxFrame&&!h.error&&(!(h.flags&Discontinuity)||(h.flags&KeyFrame));
    if(h.kind!=Ack&&h.kind!=Failure)return false;
    if(h.bytes||h.checksum||h.flags||h.sequence||h.pts)return false;
    return h.kind==Ack?(!h.error&&webcamEligible(h.mode)):h.error!=0;
}
inline std::array<uint8_t,HeaderBytes> command(uint16_t kind,uint64_t token,uint64_t session,WebcamMode mode={}){
    if(!token||!session||(kind!=Open&&kind!=Stop)||(kind==Open&&!webcamEligible(mode))||(kind==Stop&&!(mode==WebcamMode{})))throw std::invalid_argument("camera command");
    std::array<uint8_t,HeaderBytes> b{};b[0]='S';b[1]='7';b[2]='W';b[3]='C';put16(b.data()+4,1);put16(b.data()+6,kind);
    put64(b.data()+8,token);put64(b.data()+16,session);put32(b.data()+24,mode.width);put32(b.data()+28,mode.height);put32(b.data()+32,mode.fps);
    put32(b.data()+60,crc(std::span<uint8_t const>(b).first(60)));return b;
}
}
