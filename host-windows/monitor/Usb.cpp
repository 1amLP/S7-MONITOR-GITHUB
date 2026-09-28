#include "Usb.h"
#include "IoLease.h"
#include "../UsbIdentity.h"
#include <setupapi.h>
#include <initguid.h>
#include <devpkey.h>
namespace s7 {
// Buffers, event and OVERLAPPED all have the same heap lifetime. No caller
// buffer (including the 12-byte status on its stack) is submitted to Windows.
struct UsbOperation {
    Handle event;
    OVERLAPPED overlapped{};
    Bytes bytes;
    explicit UsbOperation(size_t size):event(CreateEventW(nullptr,TRUE,FALSE,nullptr)),bytes(size){
        wincheck(bool(event),"USB operation event");overlapped.hEvent=event.get();
    }
};
struct UsbState {
    Handle file;
    WINUSB_INTERFACE_HANDLE usb=nullptr;
    std::mutex mutex;
    std::array<std::shared_ptr<UsbOperation>,2> pending{}; // control, bulk
    bool failed=false,released=false;
    ~UsbState(){if(usb)WinUsb_Free(usb);}
    void begin(unsigned slot,const std::shared_ptr<UsbOperation>& op){
        std::lock_guard<std::mutex> lock(mutex);
        if(failed||released||pending[slot])throw TransportFailure(HRESULT_FROM_WIN32(ERROR_BUSY),"S7 transport must drain before reuse");
        pending[slot]=op; // Registered BEFORE submitting I/O; cancellation allocates nothing.
    }
    void finish(unsigned slot){std::lock_guard<std::mutex> lock(mutex);pending[slot].reset();}
    void poison(){std::lock_guard<std::mutex> lock(mutex);failed=true;}
    void release(){std::lock_guard<std::mutex> lock(mutex);released=true;}
    bool releasable(){
        std::lock_guard<std::mutex> lock(mutex);
        if(!released)return false;
        for(auto& op:pending){
            if(!op)continue;
            if(WaitForSingleObject(op->event.get(),0)!=WAIT_OBJECT_0)return false;
            ULONG n=0;BOOL ok=WinUsb_GetOverlappedResult(usb,&op->overlapped,&n,FALSE);
            if(!ok&&GetLastError()==ERROR_IO_INCOMPLETE)return false;
            op.reset();
        }
        return true;
    }
};
static IoLease<UsbState>& leases(){
    // Intentionally process-lifetime: DLL teardown must not free storage which
    // a stuck kernel request still owns. No detached cleanup thread or callbacks.
    // At most ONE target and TWO <=16 KiB operations can be quarantined. Reopen
    // is refused until they finish; the OS reclaims them on process termination.
    static auto* registry=new IoLease<UsbState>();return *registry;
}
Usb::Usb(const std::wstring& path,const GUID& containerId):state_(std::make_shared<UsbState>()),path_(path),containerId_(containerId){
    if(IsEqualGUID(containerId_,GUID_NULL))throw Failure(E_INVALIDARG,"S7 USB container identity is missing");
    if(!leases().available())throw TransportFailure(HRESULT_FROM_WIN32(ERROR_BUSY),"Previous S7 I/O is still owned by Windows");
    sequence.store(microseconds()); // monotonic across host reopen within a boot

    state_->file.reset(CreateFileW(path.c_str(),GENERIC_READ|GENERIC_WRITE,FILE_SHARE_READ|FILE_SHARE_WRITE,nullptr,OPEN_EXISTING,FILE_FLAG_OVERLAPPED,nullptr));
    wincheck(bool(state_->file),"Open S7 WinUSB interface");
    wincheck(WinUsb_Initialize(state_->file.get(),&state_->usb),"WinUsb_Initialize");
    try{
        USB_INTERFACE_DESCRIPTOR descriptor{};wincheck(WinUsb_QueryInterfaceSettings(state_->usb,0,&descriptor),"USB interface descriptor");
        if(descriptor.bInterfaceClass!=0xff||descriptor.bInterfaceSubClass!=0x53||descriptor.bInterfaceProtocol!=0x71||descriptor.bNumEndpoints!=1)
            throw Failure(E_INVALIDARG,"Not the S7M1 H.264 interface");
        interface_=descriptor.bInterfaceNumber;
        WINUSB_PIPE_INFORMATION pipe{};wincheck(WinUsb_QueryPipe(state_->usb,0,0,&pipe),"USB bulk pipe");
        if(pipe.PipeType!=UsbdPipeTypeBulk||USB_ENDPOINT_DIRECTION_IN(pipe.PipeId)||pipe.MaximumPacketSize!=512)
            throw Failure(E_INVALIDARG,"H.264 monitor requires USB 2.0 High-Speed bulk OUT");
        pipe_=pipe.PipeId;ULONG timeout=500;wincheck(WinUsb_SetPipePolicy(state_->usb,pipe_,PIPE_TRANSFER_TIMEOUT,sizeof(timeout),&timeout),"Set USB write timeout");
        USB_DEVICE_DESCRIPTOR device{};ULONG n=0;wincheck(WinUsb_GetDescriptor(state_->usb,USB_DEVICE_DESCRIPTOR_TYPE,0,0,reinterpret_cast<PUCHAR>(&device),sizeof(device),&n),"USB device identity");
        if(n!=sizeof(device)||!s7identity::product(device.idVendor,device.idProduct)||!device.iSerialNumber)throw Failure(E_INVALIDARG,"Unexpected S7 USB identity");
        uint8_t raw[256]{};wincheck(WinUsb_GetDescriptor(state_->usb,USB_STRING_DESCRIPTOR_TYPE,0,0,raw,sizeof(raw),&n),"USB language descriptor");
        if(n<4||raw[1]!=USB_STRING_DESCRIPTOR_TYPE)throw Failure(E_INVALIDARG,"Missing USB language");USHORT language=le16(raw+2);
        wincheck(WinUsb_GetDescriptor(state_->usb,USB_STRING_DESCRIPTOR_TYPE,device.iSerialNumber,language,raw,sizeof(raw),&n),"USB serial descriptor");
        if(n<2||raw[0]!=n||raw[1]!=USB_STRING_DESCRIPTOR_TYPE||(n&1))throw Failure(E_INVALIDARG,"Malformed USB serial");
        std::wstring serial;for(ULONG i=2;i<n;i+=2)serial.push_back(wchar_t(le16(raw+i)));
        if(serial!=TargetSerial)throw Failure(E_ACCESSDENIED,"This package is pinned to a different S7 serial");
        if(!leases().acquire(state_))throw TransportFailure(HRESULT_FROM_WIN32(ERROR_BUSY),"S7 transport is already in use");
    }catch(...){WinUsb_Free(state_->usb);state_->usb=nullptr;throw;}
}
Usb::~Usb(){state_->release();leases().collect();}
ULONG Usb::complete(BOOL result,const std::shared_ptr<UsbOperation>& operation,unsigned slot,HANDLE cancel,const TransferBudget* budget){
    DWORD error=result?ERROR_SUCCESS:GetLastError();
    auto& op=operation->overlapped;
    if(!result&&error!=ERROR_IO_PENDING){
        state_->finish(slot);state_->poison();
        throw TransportFailure(HRESULT_FROM_WIN32(error),"Submit USB operation");
    }
    if(!result){
        HANDLE handles[2]={op.hEvent,cancel};DWORD count=cancel?2:1;
        DWORD allowance=budget?budget->remainingMS(microseconds()):600;
        DWORD wait=WaitForMultipleObjects(count,handles,FALSE,allowance);
        if(wait!=WAIT_OBJECT_0){
            DWORD reason=wait==WAIT_FAILED?GetLastError():(wait==WAIT_OBJECT_0+1?ERROR_CANCELLED:ERROR_TIMEOUT);
            state_->poison();CancelIoEx(state_->file.get(),&op);
            // CancelIoEx only requests cancellation. A bounded wait does NOT
            // permit freeing op: state/lease keep it alive until completion.
            if(WaitForSingleObject(op.hEvent,100)==WAIT_OBJECT_0){
                ULONG ignored=0;BOOL done=WinUsb_GetOverlappedResult(state_->usb,&op,&ignored,FALSE);
                if(done||GetLastError()!=ERROR_IO_INCOMPLETE)state_->finish(slot);
            }
            throw TransportFailure(HRESULT_FROM_WIN32(reason),"USB deadline/cancel; transport quarantined until drained");
        }
    }
    ULONG transferred=0;BOOL done=WinUsb_GetOverlappedResult(state_->usb,&op,&transferred,FALSE);
    error=done?ERROR_SUCCESS:GetLastError();
    if(error!=ERROR_IO_INCOMPLETE)state_->finish(slot);
    if(!done){state_->poison();throw TransportFailure(HRESULT_FROM_WIN32(error),"USB completion");}
    return transferred;
}
ULONG Usb::transfer(bool input,UCHAR request,uint8_t* p,USHORT size,HANDLE cancel,USHORT value){
    std::lock_guard<std::mutex> guard(controlMutex_);
    if(cancel&&WaitForSingleObject(cancel,0)==WAIT_OBJECT_0)throw TransportFailure(HRESULT_FROM_WIN32(ERROR_CANCELLED),"Monitor stopped");
    auto operation=std::make_shared<UsbOperation>(size);
    if(!input)std::copy_n(p,size,operation->bytes.data());
    WINUSB_SETUP_PACKET setup{};setup.RequestType=input?0xc1:0x41;setup.Request=request;setup.Index=interface_;setup.Length=size;setup.Value=value;
    state_->begin(0,operation);
    ULONG n=complete(WinUsb_ControlTransfer(state_->usb,setup,operation->bytes.data(),size,nullptr,&operation->overlapped),operation,0,cancel);
    if(n>size)throw TransportFailure(E_FAIL,"Invalid USB control length");
    if(input)std::copy_n(operation->bytes.data(),n,p);
    return n;
}
Config Usb::config(HANDLE cancel){std::array<uint8_t,ConfigBytes> b{};ULONG n=transfer(true,0x51,b.data(),USHORT(b.size()),cancel);return Config::parse(b.data(),n);}
SniperControl Usb::sniper(HANDLE cancel,uint16_t ack){std::array<uint8_t,32> b{};ULONG n=transfer(true,0x55,b.data(),USHORT(b.size()),cancel,ack);return SniperControl::parse(b.data(),n);}
void Usb::status(const Config& c,uint32_t state,HRESULT error,HANDLE cancel){
    uint8_t b[12]{};put32(b,c.generation);put32(b+4,state);put32(b+8,uint32_t(error));
    if(transfer(false,0x52,b,sizeof(b),cancel)!=sizeof(b))throw TransportFailure(E_FAIL,"Short host status write");
}
void Usb::send(const Config& c,const Bytes& frame,uint64_t pts,bool idr,HANDLE cancel){
    std::lock_guard<std::mutex> lock(writeMutex_);
    auto header=frameHeader(c,frame.size(),++sequence,pts,idr);Bytes packet(header.begin(),header.end());packet.insert(packet.end(),frame.begin(),frame.end());
    const TransferBudget budget(microseconds());
    size_t offset=0;
    while(offset<packet.size()){
        if(cancel&&WaitForSingleObject(cancel,0)==WAIT_OBJECT_0){
            state_->poison();throw TransportFailure(HRESULT_FROM_WIN32(ERROR_CANCELLED),"Monitor stopped during framed transfer");
        }
        if(!budget.remainingMS(microseconds())){
            state_->poison();throw TransportFailure(HRESULT_FROM_WIN32(ERROR_TIMEOUT),"Whole monitor frame exceeded 75ms; reconnect required");
        }
        ULONG size=ULONG((std::min)(packet.size()-offset,size_t(16384)));
        auto operation=std::make_shared<UsbOperation>(size);std::copy_n(packet.data()+offset,size,operation->bytes.data());
        state_->begin(1,operation);
        ULONG n=complete(WinUsb_WritePipe(state_->usb,pipe_,operation->bytes.data(),size,nullptr,&operation->overlapped),operation,1,cancel,&budget);
        if(n!=size){state_->poison();throw TransportFailure(E_FAIL,"Short USB frame write; reconnect required");}offset+=n;
    }
    if(!budget.remainingMS(microseconds())){
        state_->poison();throw TransportFailure(HRESULT_FROM_WIN32(ERROR_TIMEOUT),"Monitor frame completed after its deadline");
    }
}
std::shared_ptr<Usb> Usb::discover(){
    if(!leases().available())throw TransportFailure(HRESULT_FROM_WIN32(ERROR_BUSY),"S7 transport awaiting safe I/O retirement");
    HDEVINFO devices=SetupDiGetClassDevsW(&MonitorInterface,nullptr,nullptr,DIGCF_PRESENT|DIGCF_DEVICEINTERFACE);
    if(devices==INVALID_HANDLE_VALUE)throw Failure(HRESULT_FROM_WIN32(GetLastError()),"Enumerate WinUSB");
    struct Guard {HDEVINFO value;~Guard(){SetupDiDestroyDeviceInfoList(value);}}guard{devices};
    std::wstring selectedPath;GUID selectedContainer{};
    for(DWORD index=0;;index++){
        SP_DEVICE_INTERFACE_DATA data{};data.cbSize=sizeof(data);
        if(!SetupDiEnumDeviceInterfaces(devices,nullptr,&MonitorInterface,index,&data)){if(GetLastError()==ERROR_NO_MORE_ITEMS)break;throw Failure(HRESULT_FROM_WIN32(GetLastError()),"Enumerate S7 interface");}
        DWORD size=0;SetupDiGetDeviceInterfaceDetailW(devices,&data,nullptr,0,&size,nullptr);
        if(size<sizeof(SP_DEVICE_INTERFACE_DETAIL_DATA_W)||size>65536)throw Failure(E_FAIL,"Invalid interface path size");
        std::vector<uint8_t> storage(size);auto detail=reinterpret_cast<SP_DEVICE_INTERFACE_DETAIL_DATA_W*>(storage.data());detail->cbSize=sizeof(*detail);
        SP_DEVINFO_DATA node{};node.cbSize=sizeof(node);
        wincheck(SetupDiGetDeviceInterfaceDetailW(devices,&data,detail,size,nullptr,&node),"Read S7 interface path");
        GUID container{};DEVPROPTYPE type=0;DWORD required=0;
        wincheck(SetupDiGetDevicePropertyW(devices,&node,&DEVPKEY_Device_ContainerId,&type,
            reinterpret_cast<PBYTE>(&container),sizeof(container),&required,0),"Read S7 USB container identity");
        if(type!=DEVPROP_TYPE_GUID||required!=sizeof(container))throw Failure(E_INVALIDARG,"Invalid S7 container property");
        // Finish candidate selection BEFORE opening WinUSB/acquiring its one
        // lifetime lease. Opening candidate #2 while #1 owns the lease used to
        // mask ambiguity as ERROR_BUSY and partially acquire the first device.
        if(!selectedPath.empty())throw Failure(E_UNEXPECTED,"Multiple matching S7 interfaces; refusing ambiguous device");
        selectedPath=detail->DevicePath;selectedContainer=container;
    }
    if(selectedPath.empty())return nullptr;
    return std::make_shared<Usb>(selectedPath,selectedContainer);
}
bool Usb::present(const std::wstring& path){
    if(path.empty())throw Failure(E_INVALIDARG,"Missing last-known S7 path");
    HDEVINFO devices=SetupDiGetClassDevsW(&MonitorInterface,nullptr,nullptr,DIGCF_PRESENT|DIGCF_DEVICEINTERFACE);
    if(devices==INVALID_HANDLE_VALUE)throw Failure(HRESULT_FROM_WIN32(GetLastError()),"Enumerate S7 presence");
    struct Guard{HDEVINFO value;~Guard(){SetupDiDestroyDeviceInfoList(value);}}guard{devices};
    for(DWORD index=0;;++index){
        SP_DEVICE_INTERFACE_DATA data{};data.cbSize=sizeof(data);
        if(!SetupDiEnumDeviceInterfaces(devices,nullptr,&MonitorInterface,index,&data)){
            if(GetLastError()==ERROR_NO_MORE_ITEMS)return false;
            throw Failure(HRESULT_FROM_WIN32(GetLastError()),"Enumerate S7 presence interface");
        }
        DWORD size=0;SetupDiGetDeviceInterfaceDetailW(devices,&data,nullptr,0,&size,nullptr);
        if(size<sizeof(SP_DEVICE_INTERFACE_DETAIL_DATA_W)||size>65536)throw Failure(E_FAIL,"Invalid presence path size");
        std::vector<uint8_t> storage(size);auto detail=reinterpret_cast<SP_DEVICE_INTERFACE_DETAIL_DATA_W*>(storage.data());detail->cbSize=sizeof(*detail);
        wincheck(SetupDiGetDeviceInterfaceDetailW(devices,&data,detail,size,nullptr,nullptr),"Read S7 presence path");
        if(_wcsicmp(path.c_str(),detail->DevicePath)==0)return true;
    }
}

}
