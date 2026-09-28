#pragma once
// Versioned HID payload shared with native/pkg/cameractl. No Windows headers.
#include "ModePolicy.h"
#include <array>
#include <cstdint>
#include <cstddef>
#include <vector>
#include <stdexcept>
namespace s7camera::selection {
inline constexpr uint8_t ReportId=8,Version=2;
inline constexpr size_t ReportSize=65;
using Report=std::array<uint8_t,ReportSize>;
enum class Sensor:uint8_t { Rear=0,Front=1 };
enum Kind:uint8_t { Status=0,Acquire=1,Release=2,Keepalive=3 };
enum Result:uint8_t { OK=0,Disabled=1,Unavailable=2,Busy=3,Stale=4,Invalid=5,Thermal=6,Unsafe=7 };
enum Flag:uint16_t { Enabled=1,USBReady=2,Streaming=4,Paused=8,OwnershipFault=16 };
inline std::vector<WebcamMode> modes(Sensor s){
 if(s==Sensor::Rear)return {{2560,1440,30},{1920,1080,60},{1920,1080,30},{1280,720,240},{1280,720,120},{1280,720,60},{1280,720,30}};
 if(s==Sensor::Front)return {{2560,1440,30},{1920,1080,30},{1280,720,30}};
 return {};
}
inline uint8_t bit(Sensor s,WebcamMode m){uint8_t n=1;for(auto v:modes(s)){if(v==m)return n;n=uint8_t(n<<1);}return 0;}
struct Message {
 uint8_t kind=Status;Sensor sensor=Sensor::Rear;uint8_t result=OK;
 uint32_t sequence=0;uint64_t token=0,generation=0;WebcamMode mode{};
 std::array<uint8_t,2> masks{};uint16_t flags=0;uint64_t owner=0;
};
inline void put(Report& b,size_t offset,uint64_t v,size_t size){for(size_t i=0;i<size;++i)b[offset+i]=uint8_t(v>>(8*i));}
inline uint64_t get(Report const& b,size_t offset,size_t size){uint64_t v=0;for(size_t i=0;i<size;++i)v|=uint64_t(b[offset+i])<<(8*i);return v;}
inline Report encode(Message const& m){
 Report b{};b[0]=ReportId;b[1]='S';b[2]='7';b[3]='S';b[4]='2';b[5]=Version;
 b[6]=m.kind;b[7]=uint8_t(m.sensor);b[8]=m.result;put(b,9,m.sequence,4);put(b,13,m.token,8);put(b,21,m.generation,8);
 put(b,29,m.mode.width,4);put(b,33,m.mode.height,4);put(b,37,m.mode.fps,4);b[41]=m.masks[0];b[42]=m.masks[1];put(b,43,m.flags,2);put(b,45,m.owner,8);return b;
}
inline bool decode(Report const& b,Message& out){
 if(b[0]!=ReportId||b[1]!='S'||b[2]!='7'||b[3]!='S'||b[4]!='2'||b[5]!=Version)return false;
 for(size_t i=53;i<b.size();++i)if(b[i])return false;
 Message m;m.kind=b[6];m.sensor=Sensor(b[7]);m.result=b[8];m.sequence=uint32_t(get(b,9,4));m.token=get(b,13,8);m.generation=get(b,21,8);
 m.mode={uint32_t(get(b,29,4)),uint32_t(get(b,33,4)),uint32_t(get(b,37,4))};m.masks={b[41],b[42]};m.flags=uint16_t(get(b,43,2));m.owner=get(b,45,8);
 if(m.kind>Keepalive||uint8_t(m.sensor)>1||m.result>Unsafe||(m.masks[0]&~127)||(m.masks[1]&~7)||(m.flags&~31))return false;
 if(m.kind!=Status){
  if(!m.token||!m.sequence||m.result||m.generation||m.masks[0]||m.masks[1]||m.flags||m.owner)return false;
  if(m.kind==Acquire){if(!bit(m.sensor,m.mode))return false;}else if(!(m.mode==WebcamMode{}))return false;
 }
 out=m;return true;
}
inline std::vector<WebcamMode> admitted(Message const& status,Sensor sensor){
 std::vector<WebcamMode> v;if(uint8_t(sensor)>1)return v;
 for(auto m:modes(sensor))if(webcamEligible(m)&&(status.masks[uint8_t(sensor)]&bit(sensor,m)))v.push_back(m);
 // Prefer native 1080p30, not the most costly rear QHD mode by enumeration order.
 for(size_t i=0;i<v.size();++i)if(v[i]==WebcamMode{1920,1080,30}){auto m=v[i];v.erase(v.begin()+static_cast<std::ptrdiff_t>(i));v.insert(v.begin(),m);break;}
 if(status.sensor==sensor)for(size_t i=0;i<v.size();++i)if(v[i]==status.mode){auto m=v[i];v.erase(v.begin()+static_cast<std::ptrdiff_t>(i));v.insert(v.begin(),m);break;}
 return v;
}
inline bool matches(Message const& m,Sensor sensor,WebcamMode mode,uint64_t token){
 return webcamEligible(mode)&&bit(sensor,mode)&&m.kind==Status&&m.sensor==sensor&&m.mode==mode&&m.owner==token&&(m.flags&Enabled)&&(m.flags&USBReady)&&!(m.flags&(Paused|OwnershipFault));
}
inline bool follows(Message const& m,WebcamMode mode,uint64_t token,uint64_t generation){
 return m.generation>=generation&&matches(m,m.sensor,mode,token);
}
}
