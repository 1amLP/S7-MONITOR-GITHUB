#pragma once
#include "HostWindows.h"
#include "Protocol.h"
#include "../DeviceConfig.h"
#include <sddl.h>
#include <wtsapi32.h>

namespace s7 {
struct SniperControl {
    uint32_t generation=0,zoom=100,x=5000,y=5000,scaleX=100,scaleY=100;
    uint16_t sequence=0,contactX=0,contactY=0;
    uint16_t rotation=0;
    bool active=false,stretch=false,down=false,mirror=false;
    static SniperControl parse(const uint8_t* p,size_t size){
        if(size!=32||std::memcmp(p,"S7S1",4)||le16(p+4)<1||le16(p+4)>3||le16(p+6)!=32||le16(p+22)>1)
            throw Failure(E_INVALIDARG,"Invalid Sniper control");
        SniperControl c;c.generation=le32(p+8);c.zoom=le16(p+12);c.x=le16(p+14);c.y=le16(p+16);
        const auto flags=le16(p+18);
        if(le16(p+4)==3){c.rotation=flags>>3;c.mirror=(flags&4)!=0;}
        else if(flags>3)throw Failure(E_INVALIDARG,"Invalid legacy Sniper flags");
        if(le16(p+4)>=2){c.scaleX=le16(p+28);c.scaleY=le16(p+30);}
        else if(le32(p+28))throw Failure(E_INVALIDARG,"Invalid legacy Sniper scale");
        c.active=(p[18]&1)!=0;c.stretch=(p[18]&2)!=0;c.sequence=le16(p+20);c.down=p[22]!=0;c.contactX=le16(p+24);c.contactY=le16(p+26);
        if(!c.generation||c.zoom<100||c.zoom>1600||c.x>10000||c.y>10000||c.contactX>32767||c.contactY>32767||c.scaleX<25||c.scaleX>400||c.scaleY<25||c.scaleY>400||c.rotation>=3600)
            throw Failure(E_INVALIDARG,"Sniper control outside bounds");
        return c;
    }
};
struct SniperRequest {
    uint32_t generation=0,width=0,height=0,zoom=100,x=5000,y=5000,stretch=0;
    uint32_t scaleX=100,scaleY=100;
    uint32_t sequence=0,contactX=0,contactY=0,down=0;
    uint32_t rotation=0,mirror=0;
    uint64_t atMS=0;
    bool valid()const {return generation&&Config::mode(width,height)&&zoom>=100&&zoom<=1600&&x<=10000&&y<=10000&&stretch<=1&&scaleX>=25&&scaleX<=400&&scaleY>=25&&scaleY<=400&&sequence<=65535&&down<=1&&contactX<=32767&&contactY<=32767&&rotation<3600&&mirror<=1;}
    bool fresh()const {const auto now=GetTickCount64();return valid()&&now>=atMS&&now-atMS<1000;}
};
constexpr uint32_t SniperMagic=0x34504353,SniperMaxBytes=2560*1440*3/2;
struct SniperShared {
    uint32_t magic=0,size=0;
    SniperRequest request;
    uint32_t frameGeneration=0,bytes=0,acceptedSequence=0;
    HRESULT error=S_OK;
    uint64_t frameTimeUS=0,frameSequence=0;
    RECT primaryDesktop{};
    uint32_t primaryGeneration=0,mappingRequested=0,mappingApplied=0;
    HRESULT mappingError=E_PENDING;
    wchar_t primaryDevice[128]{},touchDevice[512]{};
    uint8_t pixels[SniperMaxBytes];
};
// Service creates session-scoped objects. Only SYSTEM, UMDF's LocalService and
// the current console user can access desktop pixels. Never Everyone/IU.
class SniperChannel {
    Handle mapping_,mutex_;
    SniperShared* shared_=nullptr;
public:
    static std::wstring name(DWORD session,const wchar_t* suffix){return std::wstring(L"Global\\S7.Sniper.")+TargetSerial+L".v4."+std::to_wstring(session)+suffix;}
    SniperChannel(DWORD session,HANDLE userToken=nullptr){
        if(session==0xffffffff)throw Failure(E_ACCESSDENIED,"No interactive console session");
        const auto mapName=name(session,L".data"),mutexName=name(session,L".lock");
        if(userToken){
            DWORD n=0;GetTokenInformation(userToken,TokenUser,nullptr,0,&n);Bytes token(n);
            wincheck(n&&GetTokenInformation(userToken,TokenUser,token.data(),n,&n),"Sniper console user identity");
            LPWSTR sid=nullptr;wincheck(ConvertSidToStringSidW(reinterpret_cast<TOKEN_USER*>(token.data())->User.Sid,&sid),"Sniper user SID");
            const auto acl=std::wstring(L"D:P(A;;GA;;;SY)(A;;GA;;;LS)(A;;GA;;;")+sid+L")";LocalFree(sid);
            PSECURITY_DESCRIPTOR sd=nullptr;wincheck(ConvertStringSecurityDescriptorToSecurityDescriptorW(acl.c_str(),SDDL_REVISION_1,&sd,nullptr),"Sniper channel ACL");
            struct Free{PSECURITY_DESCRIPTOR sd;~Free(){LocalFree(sd);}}free{sd};
            SECURITY_ATTRIBUTES sa{sizeof(sa),sd,FALSE};
            mapping_.reset(CreateFileMappingW(INVALID_HANDLE_VALUE,&sa,PAGE_READWRITE,0,sizeof(SniperShared),mapName.c_str()));
            const auto mapError=GetLastError();wincheck(bool(mapping_),"Create Sniper channel");
            if(mapError==ERROR_ALREADY_EXISTS)throw Failure(E_ACCESSDENIED,"Unexpected pre-existing Sniper channel");
            mutex_.reset(CreateMutexW(&sa,FALSE,mutexName.c_str()));
            const auto mutexError=GetLastError();wincheck(bool(mutex_),"Create Sniper channel lock");
            if(mutexError==ERROR_ALREADY_EXISTS)throw Failure(E_ACCESSDENIED,"Unexpected pre-existing Sniper lock");
        }else{
            mapping_.reset(OpenFileMappingW(FILE_MAP_ALL_ACCESS,FALSE,mapName.c_str()));wincheck(bool(mapping_),"Open Sniper channel");
            mutex_.reset(OpenMutexW(SYNCHRONIZE|MUTEX_MODIFY_STATE,FALSE,mutexName.c_str()));wincheck(bool(mutex_),"Open Sniper lock");
        }
        shared_=static_cast<SniperShared*>(MapViewOfFile(mapping_.get(),FILE_MAP_ALL_ACCESS,0,0,sizeof(SniperShared)));
        wincheck(shared_!=nullptr,"Map Sniper channel");
        if(userToken){shared_->size=sizeof(SniperShared);shared_->magic=SniperMagic;}
        if(shared_->magic!=SniperMagic||shared_->size!=sizeof(SniperShared)){UnmapViewOfFile(shared_);shared_=nullptr;throw Failure(E_INVALIDARG,"Sniper channel version");}
    }
    ~SniperChannel(){if(shared_)UnmapViewOfFile(shared_);}
    SniperChannel(const SniperChannel&)=delete;
    template<class Fn> bool access(Fn&& fn){
        const DWORD result=WaitForSingleObject(mutex_.get(),0);
        if(result==WAIT_TIMEOUT)return false;
        if(result==WAIT_ABANDONED){ReleaseMutex(mutex_.get());throw Failure(E_ABORT,"Sniper owner exited inside channel write");}
        wincheck(result==WAIT_OBJECT_0,"Lock Sniper channel");
        struct Unlock{HANDLE h;~Unlock(){ReleaseMutex(h);}}unlock{mutex_.get()};
        fn(*shared_);return true;
    }
};
}
