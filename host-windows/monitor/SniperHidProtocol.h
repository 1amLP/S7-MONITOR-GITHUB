#pragma once
#include "Protocol.h"
namespace s7 {
constexpr uint8_t SniperFeatureID=12,SniperInputID=13,SniperMaximumID=14;
constexpr size_t SniperHidBytes=33;
enum SniperHidKind:uint8_t {HidStatus=0,HidPoint=1,HidCancel=2};
enum SniperHidStatus:uint8_t {HidOK=0,HidPending=1,HidDisabled=2,HidStale=3,HidIOError=4};
struct SniperHidMessage {
    uint8_t kind=HidStatus,status=HidOK;
    uint32_t generation=0;
    uint16_t sequence=0,x=0,y=0;
    bool down=false;
    std::array<uint8_t,SniperHidBytes> encode()const{
        std::array<uint8_t,SniperHidBytes> b{};b[0]=SniperFeatureID;std::memcpy(b.data()+1,"S7H1",4);
        b[5]=1;b[6]=kind;b[7]=status;put32(b.data()+8,generation);put16(b.data()+12,sequence);b[14]=down?1:0;put16(b.data()+16,x);put16(b.data()+18,y);return b;
    }
    static SniperHidMessage parse(const Bytes& b){
        if(b.size()!=SniperHidBytes||b[0]!=SniperFeatureID||std::memcmp(b.data()+1,"S7H1",4)||b[5]!=1||b[6]>HidCancel||b[7]>HidIOError||b[14]>1||b[15])throw Failure(E_INVALIDARG,"Invalid Sniper HID response");
        for(size_t i=20;i<b.size();++i)if(b[i])throw Failure(E_INVALIDARG,"Unknown Sniper HID flags");
        SniperHidMessage m{b[6],b[7],le32(b.data()+8),le16(b.data()+12),le16(b.data()+16),le16(b.data()+18),b[14]!=0};
        if(m.x>32767||m.y>32767||(m.kind!=HidStatus&&(!m.generation||m.status))||(m.kind==HidPoint&&!m.sequence)||(m.kind==HidCancel&&(m.down||m.x||m.y)))throw Failure(E_INVALIDARG,"Sniper HID fields outside bounds");
        return m;
    }
};
}
