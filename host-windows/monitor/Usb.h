#pragma once
#include "HostWindows.h"
#include "Protocol.h"
#include "TransferBudget.h"
#include "SniperChannel.h"
#include "../DeviceConfig.h"
#include <winusb.h>
#include <usb.h>
#include <mutex>
#include <atomic>
namespace s7 {
inline constexpr GUID MonitorInterface={0xf4469eb2,0x7dd9,0x47f1,{0xac,0x40,0x3f,0x10,0xd5,0x47,0x77,0x81}};
struct UsbState;
struct UsbOperation;
struct TransportFailure:Failure {using Failure::Failure;};
class Usb {
    std::shared_ptr<UsbState> state_;
    std::wstring path_;
    UCHAR pipe_=0,interface_=0;
    GUID containerId_{};
    std::mutex controlMutex_,writeMutex_;
    ULONG transfer(bool input,UCHAR request,uint8_t* p,USHORT size,HANDLE cancel,USHORT value=0);
    ULONG complete(BOOL result,const std::shared_ptr<UsbOperation>& operation,unsigned slot,HANDLE cancel,const TransferBudget* budget=nullptr);
public:
    std::atomic<uint64_t> sequence{0};
    Usb(const std::wstring& path,const GUID& containerId);
    ~Usb();
    Usb(const Usb&)=delete;Usb& operator=(const Usb&)=delete;
    Config config(HANDLE cancel);
    SniperControl sniper(HANDLE cancel,uint16_t ack);
    void status(const Config& c,uint32_t state,HRESULT error,HANDLE cancel);
    void send(const Config& c,const Bytes& frame,uint64_t pts,bool idr,HANDLE cancel);
    const GUID& containerId()const{return containerId_;}
    const std::wstring& path()const{return path_;}
    // A complete, successful PnP enumeration is required to prove absence.
    static bool present(const std::wstring& path);
    static std::shared_ptr<Usb> discover(); // Pinned serial, not an arbitrary phone.
};
}
