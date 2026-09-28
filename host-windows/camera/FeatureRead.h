#pragma once
#include "../monitor/IoLease.h"
#include <array>
#include <algorithm>
#include <atomic>
#include <memory>
#include <vector>
#include <mutex>
#include <winioctl.h>
#include <hidclass.h>

namespace s7camera {
// S7's Windows HID stack completes SET_FEATURE with the input length (65),
// while other stacks return zero. GET_FEATURE still requires every report byte.
constexpr bool completeFeature(size_t count,size_t size,bool write){return write?(count==0||count==size):count==size;}
static_assert(completeFeature(65,65,true)&&completeFeature(0,65,true)&&!completeFeature(64,65,true));
static_assert(completeFeature(65,65,false)&&!completeFeature(0,65,false)&&!completeFeature(64,65,false));
extern std::atomic<long> objects;
// Recursive ownership lets a whole SET/GET transaction keep its reply while
// individual transfers use the same gate. Active I/O is queued, not reported
// as an unsupported/busy camera property to FrameServer.
class FeatureTurn {
    static HANDLE mutex(){
        struct Owner {
            HANDLE value=CreateMutexW(nullptr,FALSE,nullptr);
            Owner(){if(!value)throw HRESULT_FROM_WIN32(GetLastError());}
            ~Owner(){CloseHandle(value);}
        };
        static Owner owner;
        return owner.value;
    }
    HANDLE held=nullptr;
public:
    explicit FeatureTurn(HANDLE cancel=nullptr){
        HANDLE gate=mutex();HANDLE waits[]{gate,cancel};
        const DWORD result=WaitForMultipleObjects(cancel?2:1,waits,FALSE,600);
        if(result==WAIT_ABANDONED_0){ReleaseMutex(gate);throw HRESULT_FROM_WIN32(ERROR_ABANDONED_WAIT_0);}
        if(result!=WAIT_OBJECT_0)throw HRESULT_FROM_WIN32(result==WAIT_FAILED?GetLastError():(result==WAIT_OBJECT_0+1?ERROR_CANCELLED:ERROR_BUSY));
        held=gate;
    }
    ~FeatureTurn(){if(held)ReleaseMutex(held);}
    FeatureTurn(FeatureTurn const&)=delete;
    FeatureTurn& operator=(FeatureTurn const&)=delete;
};
// A duplicate HANDLE keeps the file object alive independently of Preference.
// This object (including the report buffer) outlives every pending HID request.
struct FeatureRequest {
    HANDLE file=INVALID_HANDLE_VALUE,event=nullptr;
    OVERLAPPED overlapped{};
    std::vector<uint8_t> bytes;
    std::mutex mutex;
    bool pending=false,released=false;
    explicit FeatureRequest(HANDLE source,std::vector<uint8_t> report):bytes(std::move(report)){
        if(!DuplicateHandle(GetCurrentProcess(),source,GetCurrentProcess(),&file,0,FALSE,DUPLICATE_SAME_ACCESS))throw HRESULT_FROM_WIN32(GetLastError());
        event=CreateEventW(nullptr,TRUE,FALSE,nullptr);
        if(!event){DWORD error=GetLastError();CloseHandle(file);file=INVALID_HANDLE_VALUE;throw HRESULT_FROM_WIN32(error);}
        overlapped.hEvent=event;++objects;
    }
    ~FeatureRequest(){if(event)CloseHandle(event);if(file!=INVALID_HANDLE_VALUE)CloseHandle(file);--objects;}
    void submitted(){std::lock_guard<std::mutex> lock(mutex);pending=true;}
    void finished(){std::lock_guard<std::mutex> lock(mutex);pending=false;}
    void release(){std::lock_guard<std::mutex> lock(mutex);released=true;}
    bool releasable(){
        std::lock_guard<std::mutex> lock(mutex);
        if(!released)return false;
        if(!pending)return true;
        if(WaitForSingleObject(event,0)!=WAIT_OBJECT_0)return false;
        DWORD bytesRead=0;
        BOOL result=GetOverlappedResult(file,&overlapped,&bytesRead,FALSE);
        if(!result&&GetLastError()==ERROR_IO_INCOMPLETE)return false;
        pending=false;return true;
    }
};
inline s7::IoLease<FeatureRequest>& featureLease(){
    // Process lifetime, like monitor I/O. At most ONE bounded request may be
    // quarantined. It counts as a live DLL object until drained, so COM cannot
    // unload this registry while Windows still owns the request buffer.
    static auto* lease=new s7::IoLease<FeatureRequest>();return *lease;
}
inline std::vector<uint8_t> transferFeature(HANDLE file,std::vector<uint8_t> report,bool write,HANDLE cancel){
 if(report.empty()||report.size()>65)throw E_INVALIDARG;
    FeatureTurn turn(cancel);
    if(cancel&&WaitForSingleObject(cancel,0)==WAIT_OBJECT_0)throw HRESULT_FROM_WIN32(ERROR_CANCELLED);
    if(!featureLease().available())throw HRESULT_FROM_WIN32(ERROR_BUSY);
    auto request=std::make_shared<FeatureRequest>(file,std::move(report));
    if(!featureLease().acquire(request))throw HRESULT_FROM_WIN32(ERROR_BUSY);
    struct Guard {
        std::shared_ptr<FeatureRequest> request;
        ~Guard(){request->release();featureLease().collect();}
    } guard{request};
    request->submitted();
    BOOL result=DeviceIoControl(request->file,write?IOCTL_HID_SET_FEATURE:IOCTL_HID_GET_FEATURE,
 write?request->bytes.data():nullptr,write?DWORD(request->bytes.size()):0,
 write?nullptr:request->bytes.data(),write?0:DWORD(request->bytes.size()),nullptr,&request->overlapped);
    DWORD error=result?ERROR_SUCCESS:GetLastError();
    if(!result&&error!=ERROR_IO_PENDING){request->finished();throw HRESULT_FROM_WIN32(error);}
    if(!result){
        HANDLE waits[]={request->event,cancel};
        DWORD wait=WaitForMultipleObjects(cancel?2:1,waits,FALSE,600);
        if(wait!=WAIT_OBJECT_0){
            error=wait==WAIT_FAILED?GetLastError():(wait==WAIT_OBJECT_0+1?ERROR_CANCELLED:ERROR_TIMEOUT);
            CancelIoEx(request->file,&request->overlapped);
            if(WaitForSingleObject(request->event,100)==WAIT_OBJECT_0){
                DWORD ignored=0;
                BOOL done=GetOverlappedResult(request->file,&request->overlapped,&ignored,FALSE);
                if(done||GetLastError()!=ERROR_IO_INCOMPLETE)request->finished();
            }
            throw HRESULT_FROM_WIN32(error);
        }
    }
    DWORD count=0;
    result=GetOverlappedResult(request->file,&request->overlapped,&count,FALSE);
    error=result?ERROR_SUCCESS:GetLastError();
    if(error!=ERROR_IO_INCOMPLETE)request->finished();
    else CancelIoEx(request->file,&request->overlapped);
    if(!result)throw HRESULT_FROM_WIN32(error);
    if(!completeFeature(count,request->bytes.size(),write))throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);
    return request->bytes;
}
inline std::array<uint8_t,17> readFeature(HANDLE file,HANDLE cancel){
 std::vector<uint8_t> request(17);request[0]=7;
 auto v=transferFeature(file,std::move(request),false,cancel);
 std::array<uint8_t,17> out{};std::copy(v.begin(),v.end(),out.begin());return out;
}
}
