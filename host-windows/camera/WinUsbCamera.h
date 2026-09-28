#pragma once
#include "Camera.h"
#include "CaptureWire.h"
#include "DeviceIdentity.h"
#include "UsbIdentity.h"
#include "../monitor/IoLease.h"
#include <winusb.h>
#include <array>
#include <atomic>
#include <memory>
#include <mutex>
#include <vector>

namespace s7camera {
extern std::atomic<long> objects;
inline constexpr GUID CameraTransportGuid={0xd8718f42,0x69a7,0x4ec2,{0xa0,0xe5,0x44,0x3a,0x07,0xc1,0x53,0x72}};
inline std::wstring cameraTransportPath(GUID const& container){
    const auto set=SetupDiGetClassDevsW(&CameraTransportGuid,nullptr,nullptr,DIGCF_PRESENT|DIGCF_DEVICEINTERFACE);
    if(set==INVALID_HANDLE_VALUE)throw HRESULT_FROM_WIN32(GetLastError());
    struct Guard{HDEVINFO s;~Guard(){SetupDiDestroyDeviceInfoList(s);}} guard{set};
    std::wstring chosen;
    for(DWORD i=0;;++i){
        SP_DEVICE_INTERFACE_DATA item{sizeof(item)};
        if(!SetupDiEnumDeviceInterfaces(set,nullptr,&CameraTransportGuid,i,&item)){if(GetLastError()==ERROR_NO_MORE_ITEMS)break;throw HRESULT_FROM_WIN32(GetLastError());}
        DWORD bytes=0;SetupDiGetDeviceInterfaceDetailW(set,&item,nullptr,0,&bytes,nullptr);
        if(bytes<sizeof(SP_DEVICE_INTERFACE_DETAIL_DATA_W)||bytes>65536)throw E_FAIL;
        std::vector<BYTE> data(bytes);auto detail=reinterpret_cast<SP_DEVICE_INTERFACE_DETAIL_DATA_W*>(data.data());detail->cbSize=sizeof(*detail);
        SP_DEVINFO_DATA node{sizeof(node)};
        if(!SetupDiGetDeviceInterfaceDetailW(set,&item,detail,bytes,nullptr,&node))throw HRESULT_FROM_WIN32(GetLastError());
        std::wstring path=detail->DevicePath;
        if(!s7UsbFunction(path)||!IsEqualGUID(containerForDevice(set,node),container))continue;
        if(!chosen.empty())throw HRESULT_FROM_WIN32(ERROR_DUP_NAME);chosen=std::move(path);
    }
    if(chosen.empty())throw HRESULT_FROM_WIN32(ERROR_DEVICE_NOT_CONNECTED);
    return chosen;
}
struct CameraUsbOperation {
    HANDLE event=CreateEventW(nullptr,TRUE,FALSE,nullptr);
    OVERLAPPED overlapped{};
    std::vector<uint8_t> bytes;
    explicit CameraUsbOperation(size_t n):bytes(n){if(!event)throw HRESULT_FROM_WIN32(GetLastError());overlapped.hEvent=event;}
    ~CameraUsbOperation(){CloseHandle(event);}
};
struct CameraUsbState {
    HANDLE file=INVALID_HANDLE_VALUE;
    WINUSB_INTERFACE_HANDLE usb=nullptr;
    std::mutex mutex;
    std::array<std::shared_ptr<CameraUsbOperation>,2> pending{};
    bool released=false,poisoned=false;
    CameraUsbState(){++objects;}
    ~CameraUsbState(){if(usb)WinUsb_Free(usb);if(file!=INVALID_HANDLE_VALUE)CloseHandle(file);--objects;}
    void begin(unsigned slot,std::shared_ptr<CameraUsbOperation> const& op){
        std::lock_guard lock(mutex);if(released||poisoned||pending[slot])throw HRESULT_FROM_WIN32(ERROR_BUSY);pending[slot]=op;
    }
    void finish(unsigned slot){std::lock_guard lock(mutex);pending[slot].reset();}
    void release(){std::lock_guard lock(mutex);released=true;}
    bool releasable(){
        std::lock_guard lock(mutex);if(!released)return false;
        for(auto& op:pending){
            if(!op)continue;if(WaitForSingleObject(op->event,0)!=WAIT_OBJECT_0)return false;
            ULONG n=0;const BOOL ok=WinUsb_GetOverlappedResult(usb,&op->overlapped,&n,FALSE);
            if(!ok&&GetLastError()==ERROR_IO_INCOMPLETE)return false;op.reset();
        }
        return true;
    }
};
inline s7::IoLease<CameraUsbState>& cameraUsbLease(){static auto* value=new s7::IoLease<CameraUsbState>();return *value;}

class WinUsbCamera {
    std::shared_ptr<CameraUsbState> state=std::make_shared<CameraUsbState>();
    UCHAR input=0,output=0;
    uint64_t token=0,session=0,lastSequence=0,lastPTS=0;
    bool opened=false,havePTS=false,acknowledged=false;
    WebcamMode selected{};
    std::vector<uint8_t> received;
    size_t offset=0;
    static void win(BOOL ok){if(!ok)throw HRESULT_FROM_WIN32(GetLastError());}
    ULONG transfer(unsigned slot,std::shared_ptr<CameraUsbOperation> const& op,HANDLE cancel,DWORD timeout){
        state->begin(slot,op);
        BOOL ok=slot==0?WinUsb_ReadPipe(state->usb,input,op->bytes.data(),ULONG(op->bytes.size()),nullptr,&op->overlapped):
                       WinUsb_WritePipe(state->usb,output,op->bytes.data(),ULONG(op->bytes.size()),nullptr,&op->overlapped);
        DWORD error=ok?ERROR_SUCCESS:GetLastError();
        if(!ok&&error!=ERROR_IO_PENDING){state->finish(slot);throw HRESULT_FROM_WIN32(error);}
        if(!ok){
            HANDLE waits[]{op->event,cancel};const DWORD wait=WaitForMultipleObjects(cancel?2:1,waits,FALSE,timeout);
            if(wait!=WAIT_OBJECT_0){
                const DWORD cause=wait==WAIT_FAILED?GetLastError():(wait==WAIT_OBJECT_0+1?ERROR_CANCELLED:ERROR_TIMEOUT);
                CancelIoEx(state->file,&op->overlapped);
                if(WaitForSingleObject(op->event,1000)==WAIT_OBJECT_0){
                    ULONG ignored=0;ok=WinUsb_GetOverlappedResult(state->usb,&op->overlapped,&ignored,FALSE);
                    if(ok||GetLastError()!=ERROR_IO_INCOMPLETE)state->finish(slot);
                }
                {std::lock_guard lock(state->mutex);if(state->pending[slot])state->poisoned=true;}
                throw HRESULT_FROM_WIN32(cause);
            }
        }
        ULONG n=0;ok=WinUsb_GetOverlappedResult(state->usb,&op->overlapped,&n,FALSE);error=ok?0:GetLastError();
        if(error!=ERROR_IO_INCOMPLETE)state->finish(slot);
        if(!ok)throw HRESULT_FROM_WIN32(error);
        if(n>op->bytes.size())throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);return n;
    }
    void send(uint16_t kind,HANDLE cancel){
        auto packet=wire::command(kind,token,session,kind==wire::Open?selected:WebcamMode{});
        auto op=std::make_shared<CameraUsbOperation>(packet.size());std::copy(packet.begin(),packet.end(),op->bytes.begin());
        if(transfer(1,op,cancel,1000)!=packet.size())throw HRESULT_FROM_WIN32(ERROR_WRITE_FAULT);
    }
    void fill(HANDLE cancel,ULONGLONG deadline){
        if(offset){received.erase(received.begin(),received.begin()+offset);offset=0;}
        if(received.size()>wire::MaxFrame+wire::HeaderBytes+1)throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);
        const auto now=GetTickCount64();if(now>=deadline)throw HRESULT_FROM_WIN32(ERROR_TIMEOUT);
        auto op=std::make_shared<CameraUsbOperation>(16384);
        const auto n=transfer(0,op,cancel,DWORD(std::min<ULONGLONG>(deadline-now,5000)));
        received.insert(received.end(),op->bytes.begin(),op->bytes.begin()+n);
    }
public:
    struct Packet{wire::Header header;std::vector<uint8_t> bytes;};
    WinUsbCamera(GUID const& container,uint64_t lease,WebcamMode mode):token(lease),selected(mode){
        if(!token||!webcamEligible(mode))throw E_INVALIDARG;
        if(!cameraUsbLease().acquire(state))throw HRESULT_FROM_WIN32(ERROR_BUSY);
        try{
        GUID id{};check(CoCreateGuid(&id));std::memcpy(&session,&id,sizeof(session));if(!session)session=1;
        const auto path=cameraTransportPath(container);
        state->file=CreateFileW(path.c_str(),GENERIC_READ|GENERIC_WRITE,FILE_SHARE_READ|FILE_SHARE_WRITE,nullptr,OPEN_EXISTING,FILE_FLAG_OVERLAPPED,nullptr);
        if(state->file==INVALID_HANDLE_VALUE)throw HRESULT_FROM_WIN32(GetLastError());
        win(WinUsb_Initialize(state->file,&state->usb));
        USB_INTERFACE_DESCRIPTOR desc{};win(WinUsb_QueryInterfaceSettings(state->usb,0,&desc));
        if(desc.bInterfaceClass!=0xff||desc.bInterfaceSubClass!=0x53||desc.bInterfaceProtocol!=0x72||desc.bNumEndpoints!=2)throw E_INVALIDARG;
        for(UCHAR i=0;i<desc.bNumEndpoints;++i){
            WINUSB_PIPE_INFORMATION pipe{};win(WinUsb_QueryPipe(state->usb,0,i,&pipe));
            if(pipe.PipeType!=UsbdPipeTypeBulk||pipe.MaximumPacketSize!=512)throw E_INVALIDARG;
            auto& target=(pipe.PipeId&0x80)?input:output;if(target)throw E_INVALIDARG;target=pipe.PipeId;
        }
        if(!input||!output)throw E_INVALIDARG;
        UCHAR no=FALSE;win(WinUsb_SetPipePolicy(state->usb,input,IGNORE_SHORT_PACKETS,sizeof(no),&no));
        win(WinUsb_AbortPipe(state->usb,input));win(WinUsb_FlushPipe(state->usb,input));
        received.reserve(wire::MaxFrame+wire::HeaderBytes+16384);
        }catch(...){state->release();cameraUsbLease().collect();throw;}
    }
    void open(HANDLE cancel){send(wire::Open,cancel);opened=true;}
    Packet next(HANDLE cancel){
        const auto deadline=GetTickCount64()+5000;size_t discarded=0;
        for(;;){
            while(received.size()-offset<wire::HeaderBytes)fill(cancel,deadline);
            wire::Header h;
            while(received.size()-offset>=wire::HeaderBytes&&!wire::parse(std::span<uint8_t const>(received).subspan(offset,wire::HeaderBytes),h)){
                ++offset;if(++discarded>wire::MaxFrame+wire::HeaderBytes)throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);
            }
            if(received.size()-offset<wire::HeaderBytes)continue;
            while(received.size()-offset<h.packetBytes())fill(cancel,deadline);
            auto payload=std::span<uint8_t const>(received).subspan(offset+wire::HeaderBytes,h.bytes);
            if(h.packetBytes()>wire::HeaderBytes+h.bytes&&received[offset+h.packetBytes()-1]!=0)throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);
            if(h.kind==wire::Frame&&wire::crc(payload)!=h.checksum){
                if(h.session==session)throw HRESULT_FROM_WIN32(ERROR_CRC);
                ++offset;if(++discarded>wire::MaxFrame+wire::HeaderBytes)throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);continue;
            }
            offset+=h.packetBytes();
            if(h.session!=session)continue;
            if(h.kind==wire::Failure)throw HRESULT_FROM_WIN32(ERROR_INVALID_STATE);
            if(h.kind==wire::Ack){if(!(h.mode==selected))throw MF_E_INVALIDMEDIATYPE;acknowledged=true;continue;}
            const bool boundary=(h.flags&(wire::Discontinuity|wire::KeyFrame))==(wire::Discontinuity|wire::KeyFrame);
            if(!acknowledged||h.sequence!=lastSequence+1||(!(h.mode==selected)&&!boundary)||
                (havePTS&&h.pts<=lastPTS))throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);
            if(!havePTS&&!(h.flags&wire::KeyFrame))throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);
            selected=h.mode;lastSequence=h.sequence;lastPTS=h.pts;havePTS=true;
            return Packet{h,std::vector<uint8_t>(payload.begin(),payload.end())};
        }
    }
    HRESULT close()noexcept{
        if(!state)return S_OK;
        if(opened){guarded([&]{send(wire::Stop,nullptr);});opened=false;}
        state->release();const bool done=state->releasable();cameraUsbLease().collect();
        if(!done)return HRESULT_FROM_WIN32(ERROR_IO_INCOMPLETE);
        state.reset();return S_OK;
    }
    ~WinUsbCamera(){close();}
};
}
