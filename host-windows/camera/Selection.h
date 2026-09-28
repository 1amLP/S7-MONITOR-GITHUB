#pragma once
#include "Camera.h"
#include "Preference.h"
#include "SelectionProtocol.h"
#include <limits>
namespace s7camera {
class CameraControl {
 Preference device{2,static_cast<USHORT>(selection::ReportSize)};
 std::vector<uint8_t> exchange(std::vector<uint8_t> const& report,bool write,HANDLE cancel){
  const auto deadline=GetTickCount64()+300;
  while(true){
   try{return transferFeature(device.handle(),report,write,cancel);}
   catch(HRESULT hr){
    if(hr!=HRESULT_FROM_WIN32(ERROR_BUSY)||GetTickCount64()>=deadline)throw;
    if(cancel){DWORD w=WaitForSingleObject(cancel,10);if(w==WAIT_OBJECT_0)throw HRESULT_FROM_WIN32(ERROR_CANCELLED);if(w==WAIT_FAILED)throw HRESULT_FROM_WIN32(GetLastError());}
    else Sleep(10);
   }
  }
 }
public:
 GUID const& containerId()const noexcept{return device.containerId();}
 selection::Message status(HANDLE cancel=nullptr){
  std::vector<uint8_t> input(selection::ReportSize);input[0]=selection::ReportId;
  auto bytes=exchange(input,false,cancel);
  selection::Report report{};std::copy(bytes.begin(),bytes.end(),report.begin());selection::Message result;
  if(!selection::decode(report,result)||result.kind!=selection::Status)throw HRESULT_FROM_WIN32(ERROR_REVISION_MISMATCH);
  return result;
 }
 selection::Message send(selection::Message command,HANDLE cancel=nullptr){
  FeatureTurn turn(cancel);
  auto bytes=selection::encode(command);
  exchange(std::vector<uint8_t>(bytes.begin(),bytes.end()),true,cancel);
  auto result=status(cancel);
  if(result.sequence!=command.sequence||result.token!=command.token)throw HRESULT_FROM_WIN32(ERROR_BUSY);
  return result;
 }
};
inline HRESULT selectionError(uint8_t result){
 switch(result){
 case selection::OK:return S_OK;
 case selection::Disabled:return E_ACCESSDENIED;
 case selection::Unavailable:return MF_E_INVALIDMEDIATYPE;
 case selection::Busy:return HRESULT_FROM_WIN32(ERROR_BUSY);
 case selection::Stale:return HRESULT_FROM_WIN32(ERROR_INVALID_STATE);
 case selection::Thermal:return HRESULT_FROM_WIN32(ERROR_RETRY);
 case selection::Unsafe:return HRESULT_FROM_WIN32(ERROR_DEVICE_HARDWARE_ERROR);
 default:return E_INVALIDARG;
 }
}
// SET_FEATURE and the following GET_FEATURE are not atomic across FrameServer
// processes. Another rear/front endpoint can replace the single device ACK in
// between. Retry ERROR_BUSY with a fresh sequence; all other failures stay fatal.
template<class Attempt,class Pause>
selection::Message retryBusyExchange(DWORD budgetMS,Attempt attempt,Pause pause){
 const auto started=GetTickCount64();
 while(true){
  selection::Message response;
  const HRESULT hr=attempt(response);
  if(SUCCEEDED(hr))return response;
  const bool retry=hr==HRESULT_FROM_WIN32(ERROR_BUSY)&&GetTickCount64()-started<budgetMS;
  if(!retry)check(hr);
  pause();
 }
}
// Lifetime is Stream::decode(), not ActivateObject()/enumeration. Both camera
// endpoints share the phone-side owner, including across FrameServer processes.
class CameraSelection {
 CameraControl control;selection::Sensor sensor;WebcamMode mode;uint64_t token=0,generation=0;
 uint32_t sequence=0;bool attempted=false,acquired=false;
 selection::Message command(uint8_t kind){
  if(sequence==std::numeric_limits<uint32_t>::max())throw HRESULT_FROM_WIN32(ERROR_INVALID_STATE);
  selection::Message c;c.kind=kind;c.sensor=sensor;c.token=token;c.sequence=++sequence;
  if(kind==selection::Acquire)c.mode=mode;return c;
 }
 selection::Message exchange(uint8_t kind,HANDLE cancel,DWORD budgetMS,DWORD pauseMS){
  return retryBusyExchange(budgetMS,[&](selection::Message& response){
   return guarded([&]{response=control.send(command(kind),cancel);check(selectionError(response.result));});
  },[&]{
   if(!cancel){Sleep(pauseMS);return;}
   const DWORD wait=WaitForSingleObject(cancel,pauseMS);
   if(wait==WAIT_OBJECT_0)throw HRESULT_FROM_WIN32(ERROR_CANCELLED);
   if(wait==WAIT_FAILED)throw HRESULT_FROM_WIN32(GetLastError());
  });
 }
public:
 GUID const& containerId()const noexcept{return control.containerId();}
 uint64_t leaseToken()const noexcept{return acquired?token:0;}
 CameraSelection(selection::Sensor s,WebcamMode m):sensor(s),mode(m){
  GUID value{};check(CoCreateGuid(&value));std::memcpy(&token,&value,sizeof(token));if(!token)token=1;
 }
 ~CameraSelection(){if(attempted){HRESULT hr=guarded([&]{exchange(selection::Release,nullptr,300,10);});if(FAILED(hr))OutputDebugStringA("S7 camera selection release failed; device retains busy/idle lease policy\n");}}
 CameraSelection(CameraSelection const&)=delete;
 void acquire(HANDLE cancel){
  attempted=true;
  const auto r=exchange(selection::Acquire,cancel,3000,50);
  if(!selection::matches(r,sensor,mode,token))throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);
  generation=r.generation;acquired=true;
 }
 void heartbeat(HANDLE cancel){
  if(!acquired)throw MF_E_NOT_INITIALIZED;
  const auto r=exchange(selection::Keepalive,cancel,300,10);
  if(!selection::follows(r,r.mode,token,generation))throw HRESULT_FROM_WIN32(ERROR_INVALID_STATE);
  if(!(r.masks[uint8_t(r.sensor)]&selection::bit(r.sensor,r.mode)))throw MF_E_INVALIDMEDIATYPE;
  sensor=r.sensor;mode=r.mode;generation=r.generation;
 }
};
}
