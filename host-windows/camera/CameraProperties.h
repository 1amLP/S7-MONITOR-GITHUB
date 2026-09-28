#pragma once
#include "Preference.h"
#include "CameraTrace.h"
#include <ks.h>
#include <ksmedia.h>
#include <array>
#include <cmath>
#include <cstdio>
#include <limits>
#include <memory>
#include <mutex>

namespace s7camera {
namespace property {
constexpr uint8_t ReportId=11,Query=1,Set=2,Reply=3,ActiveSensor=255;
constexpr uint8_t Exposure=1,ISO=2,Focus=3,WhiteBalance=4,AELock=5,AWBLock=6,Compensation=7,Zoom=8,Mirror=9,Rotation=10;
constexpr uint8_t PowerLine=11;
constexpr uint8_t Brightness=12,Contrast=13,Gamma=14,Sharpness=15,Temperature=16;
constexpr uint32_t Auto=1,Manual=2,Locked=4,Continuous=8;
using Report=std::array<uint8_t,65>;
struct Value {int64_t value=0,min=0,max=0,step=0;uint32_t flags=0;};
template<class T> T read(Report const& b,size_t offset){T value{};std::memcpy(&value,b.data()+offset,sizeof(value));return value;}
template<class T> void write(Report& b,size_t offset,T value){std::memcpy(b.data()+offset,&value,sizeof(value));}
inline Report command(uint8_t kind,uint8_t id,uint8_t sensor,uint32_t sequence,uint64_t token,int64_t value=0,uint32_t flags=0){
    Report b{};b[0]=ReportId;std::memcpy(b.data()+1,"S7P1",4);b[5]=kind;b[6]=id;b[7]=sensor;
    write(b,9,sequence);write(b,13,token);write(b,21,value);write(b,53,flags);return b;
}
inline HRESULT result(uint8_t status){
    switch(status){case 0:return S_OK;case 2:return HRESULT_FROM_WIN32(ERROR_NOT_SUPPORTED);case 3:return E_INVALIDARG;
    case 1:case 4:return HRESULT_FROM_WIN32(ERROR_BUSY);default:return E_FAIL;}
}
// This channel carries only metadata. Each GET reads the phone's current state.
class Channel {
    Preference device{2,65};uint64_t token=0;uint32_t sequence=0;
public:
    Channel(){GUID id{};check(CoCreateGuid(&id));std::memcpy(&token,&id,sizeof(token));if(!token)token=1;}
    Value exchange(uint8_t kind,uint8_t id,uint8_t sensor,int64_t value=0,uint32_t flags=0){
        FeatureTurn turn;
        if(sequence==std::numeric_limits<uint32_t>::max())throw E_UNEXPECTED;
        const auto request=command(kind,id,sensor,++sequence,token,value,flags);
        std::vector<uint8_t> get(65);get[0]=ReportId;
        const auto deadline=GetTickCount64()+1500;
        bool sent=false;
        while(true){
            if(!sent){transferFeature(device.handle(),std::vector<uint8_t>(request.begin(),request.end()),true,nullptr);sent=true;}
            const auto bytes=transferFeature(device.handle(),get,false,nullptr);
            if(bytes.size()!=65)throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);
            Report reply{};std::copy(bytes.begin(),bytes.end(),reply.begin());
            if(reply[0]!=ReportId||std::memcmp(reply.data()+1,"S7P1",4)||reply[5]!=Reply)throw HRESULT_FROM_WIN32(ERROR_REVISION_MISMATCH);
            if(reply[8]>5||std::any_of(reply.begin()+57,reply.end(),[](uint8_t b){return b!=0;}))throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);
            if(read<uint64_t>(reply,13)==token&&read<uint32_t>(reply,9)==sequence){
                if(reply[6]!=id)throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);
                if(reply[8]!=1){check(result(reply[8]));return {read<int64_t>(reply,21),read<int64_t>(reply,29),read<int64_t>(reply,37),read<int64_t>(reply,45),read<uint32_t>(reply,53)};}
            }else{sent=false;}
            if(GetTickCount64()>=deadline)throw HRESULT_FROM_WIN32(ERROR_TIMEOUT);
            Sleep(15);
        }
    }
};
}

class CameraProperties {
    std::mutex mutex;std::unique_ptr<property::Channel> channel;
    property::Value exchange(uint8_t kind,uint8_t id,uint8_t sensor,int64_t value=0,uint32_t flags=0){
        if(!channel)channel=std::make_unique<property::Channel>();
        try{return channel->exchange(kind,id,sensor,value,flags);}catch(...){channel.reset();throw;}
    }
    static void copyResult(void* data,ULONG size,ULONG* returned,void const* value,ULONG needed){
        *returned=needed;if(!data||size<needed)throw HRESULT_FROM_WIN32(ERROR_MORE_DATA);std::memcpy(data,value,needed);
    }
    static int64_t exposureNS(LONG exponent){
        if(exponent < -30 || exponent > 0)throw E_INVALIDARG;
        return static_cast<int64_t>(std::llround(std::ldexp(1e9,exponent)));
    }
    static LONG exposureStep(int64_t ns){return static_cast<LONG>(std::llround(std::log2(static_cast<double>(ns)/1e9)));}
    static void support(KSPROPERTY const& p,property::Value const& v,LONG defaultValue,void* data,ULONG size,ULONG* returned){
        constexpr ULONG access=KSPROPERTY_TYPE_GET|KSPROPERTY_TYPE_SET|KSPROPERTY_TYPE_BASICSUPPORT|KSPROPERTY_TYPE_DEFAULTVALUES;
        if(size<sizeof(KSPROPERTY_DESCRIPTION)){copyResult(data,size,returned,&access,sizeof(access));return;}
        struct Support {KSPROPERTY_DESCRIPTION description;KSPROPERTY_MEMBERSHEADER members;KSPROPERTY_STEPPING_LONG range;KSPROPERTY_MEMBERSHEADER defaults;LONG value;} s{};
        s.description.AccessFlags=access;s.description.DescriptionSize=sizeof(s);
        s.description.PropTypeSet.Set=KSPROPTYPESETID_General;s.description.PropTypeSet.Id=VT_I4;s.description.MembersListCount=2;
        s.members.MembersFlags=KSPROPERTY_MEMBER_STEPPEDRANGES;s.members.MembersSize=sizeof(s.range);s.members.MembersCount=1;
        s.range.SteppingDelta=static_cast<ULONG>(v.step);s.range.Bounds.SignedMinimum=static_cast<LONG>(v.min);s.range.Bounds.SignedMaximum=static_cast<LONG>(v.max);
        s.defaults.MembersFlags=KSPROPERTY_MEMBER_VALUES;s.defaults.MembersSize=sizeof(LONG);s.defaults.MembersCount=1;s.defaults.Flags=KSPROPERTY_MEMBER_FLAG_DEFAULT;
        s.value=std::clamp(defaultValue,static_cast<LONG>(v.min),static_cast<LONG>(v.max));
        if(p.Flags!=KSPROPERTY_TYPE_BASICSUPPORT&&p.Flags!=KSPROPERTY_TYPE_DEFAULTVALUES)throw E_INVALIDARG;
        if(size<sizeof(s)){copyResult(data,size,returned,&s.description,sizeof(s.description));return;}
        copyResult(data,size,returned,&s,sizeof(s));
    }
public:
    HRESULT invoke(PKSPROPERTY request,ULONG requestBytes,void* data,ULONG size,ULONG* returned,uint8_t sensor){const HRESULT result=guarded([&]{
        if(!returned)throw E_POINTER;*returned=0;
        if(!request||requestBytes<sizeof(KSPROPERTY))throw E_INVALIDARG;
        const KSPROPERTY p=*request;
        if(p.Flags==KSPROPERTY_TYPE_SETSUPPORT){
            if(p.Set!=PROPSETID_VIDCAP_CAMERACONTROL&&p.Set!=PROPSETID_VIDCAP_VIDEOPROCAMP&&p.Set!=PROPSETID_VIDCAP_VIDEOCONTROL&&p.Set!=KSPROPERTYSETID_ExtendedCameraControl)throw HRESULT_FROM_WIN32(ERROR_NOT_SUPPORTED);
            return;
        }
        if(p.Flags!=KSPROPERTY_TYPE_GET&&p.Flags!=KSPROPERTY_TYPE_SET&&p.Flags!=KSPROPERTY_TYPE_BASICSUPPORT&&p.Flags!=KSPROPERTY_TYPE_DEFAULTVALUES)throw HRESULT_FROM_WIN32(ERROR_NOT_SUPPORTED);
        if(p.Set==PROPSETID_VIDCAP_VIDEOCONTROL){
            if(p.Id!=KSPROPERTY_VIDEOCONTROL_CAPS&&p.Id!=KSPROPERTY_VIDEOCONTROL_MODE)throw HRESULT_FROM_WIN32(ERROR_NOT_SUPPORTED);
            if(requestBytes>=sizeof(KSPROPERTY)+sizeof(ULONG)){
                ULONG index=0;std::memcpy(&index,reinterpret_cast<BYTE const*>(request)+sizeof(KSPROPERTY),sizeof(index));if(index)throw MF_E_INVALIDSTREAMNUMBER;
            }
            if(p.Flags==KSPROPERTY_TYPE_BASICSUPPORT){ULONG access=KSPROPERTY_TYPE_GET|KSPROPERTY_TYPE_BASICSUPPORT;if(p.Id==KSPROPERTY_VIDEOCONTROL_MODE)access|=KSPROPERTY_TYPE_SET;copyResult(data,size,returned,&access,sizeof(access));return;}
            if(p.Flags==KSPROPERTY_TYPE_DEFAULTVALUES)throw HRESULT_FROM_WIN32(ERROR_NOT_SUPPORTED);
            if(p.Id==KSPROPERTY_VIDEOCONTROL_CAPS){
                if(p.Flags!=KSPROPERTY_TYPE_GET)throw E_INVALIDARG;
                KSPROPERTY_VIDEOCONTROL_CAPS_S caps{p,0,KS_VideoControlFlag_FlipHorizontal};copyResult(data,size,returned,&caps,sizeof(caps));return;
            }
            KSPROPERTY_VIDEOCONTROL_MODE_S mode{p,0,0};*returned=sizeof(mode);
            if(!data||size<sizeof(mode))throw HRESULT_FROM_WIN32(ERROR_MORE_DATA);
            std::lock_guard lock(mutex);
            if(p.Flags==KSPROPERTY_TYPE_SET){
                std::memcpy(&mode,data,sizeof(mode));if(mode.StreamIndex||(mode.Mode&~KS_VideoControlFlag_FlipHorizontal))throw E_INVALIDARG;
                exchange(property::Set,property::Mirror,sensor,mode.Mode!=0,property::Manual);
            }
            mode={p,0,exchange(property::Query,property::Mirror,sensor).value?KS_VideoControlFlag_FlipHorizontal:0};
            copyResult(data,size,returned,&mode,sizeof(mode));return;
        }
        const bool zoom=p.Set==KSPROPERTYSETID_ExtendedCameraControl&&p.Id==KSPROPERTY_CAMERACONTROL_EXTENDED_ZOOM;
        uint8_t id=0;
        if(zoom)id=property::Zoom;
        else if(p.Set==PROPSETID_VIDCAP_VIDEOPROCAMP){
            if(p.Id==KSPROPERTY_VIDEOPROCAMP_GAIN)id=property::ISO;
            if(p.Id==KSPROPERTY_VIDEOPROCAMP_POWERLINE_FREQUENCY)id=property::PowerLine;
            if(p.Id==KSPROPERTY_VIDEOPROCAMP_BRIGHTNESS)id=property::Brightness;
            if(p.Id==KSPROPERTY_VIDEOPROCAMP_CONTRAST)id=property::Contrast;
            if(p.Id==KSPROPERTY_VIDEOPROCAMP_GAMMA)id=property::Gamma;
            if(p.Id==KSPROPERTY_VIDEOPROCAMP_SHARPNESS)id=property::Sharpness;
            if(p.Id==KSPROPERTY_VIDEOPROCAMP_WHITEBALANCE)id=property::Temperature;
        }
        else if(p.Set==PROPSETID_VIDCAP_CAMERACONTROL){
            switch(p.Id){case KSPROPERTY_CAMERACONTROL_EXPOSURE:id=property::Exposure;break;
                case KSPROPERTY_CAMERACONTROL_FOCUS:id=property::Focus;break;
                case KSPROPERTY_CAMERACONTROL_ROLL:id=property::Rotation;break;}
        }
        if(!id)throw HRESULT_FROM_WIN32(ERROR_NOT_SUPPORTED);
        std::lock_guard lock(mutex);
        auto v=exchange(property::Query,id,sensor);
        if(zoom){
            if(p.Flags==KSPROPERTY_TYPE_DEFAULTVALUES)throw HRESULT_FROM_WIN32(ERROR_NOT_SUPPORTED);
            if(p.Flags==KSPROPERTY_TYPE_BASICSUPPORT){const ULONG access=KSPROPERTY_TYPE_GET|KSPROPERTY_TYPE_SET|KSPROPERTY_TYPE_BASICSUPPORT;copyResult(data,size,returned,&access,sizeof(access));return;}
            struct Zoom {KSCAMERA_EXTENDEDPROP_HEADER header;KSCAMERA_EXTENDEDPROP_VIDEOPROCSETTING setting;} z{};
            *returned=sizeof(z);if(!data||size<sizeof(z))throw HRESULT_FROM_WIN32(ERROR_MORE_DATA);
            if(p.Flags==KSPROPERTY_TYPE_SET){
                std::memcpy(&z,data,sizeof(z));
                if(z.header.Version!=1||z.header.PinId!=KSCAMERA_EXTENDEDPROP_FILTERSCOPE||z.header.Size!=sizeof(z)||z.header.Flags>KSCAMERA_EXTENDEDPROP_ZOOM_DIRECT||z.setting.Mode)throw E_INVALIDARG;
                const int64_t q16=z.setting.VideoProc.Value.ul;
                if(q16*100%65536)throw E_INVALIDARG;
                v=exchange(property::Set,id,sensor,q16*100/65536,property::Manual);
            }
            z={};z.header.Version=1;z.header.PinId=KSCAMERA_EXTENDEDPROP_FILTERSCOPE;z.header.Size=sizeof(z);
            z.header.Flags=z.header.Capability=KSCAMERA_EXTENDEDPROP_ZOOM_DIRECT;
            z.setting.Min=static_cast<LONG>(v.min*65536/100);z.setting.Max=static_cast<LONG>(v.max*65536/100);z.setting.Step=static_cast<LONG>(v.step*65536/100);
            z.setting.VideoProc.Value.ul=static_cast<ULONG>(v.value*65536/100);copyResult(data,size,returned,&z,sizeof(z));return;
        }
        const auto native=v;
        if(id==property::ISO&&(v.flags>>8&property::Manual)==0)throw HRESULT_FROM_WIN32(ERROR_NOT_SUPPORTED);
        if(id==property::Exposure){
            // Legacy KS represents shutter in log2 seconds, not milliseconds.
            if((v.flags>>8&property::Manual)==0)throw HRESULT_FROM_WIN32(ERROR_NOT_SUPPORTED);
            v.min=static_cast<LONG>(std::ceil(std::log2(static_cast<double>(v.min)/1e9)));
            v.max=static_cast<LONG>(std::floor(std::log2(static_cast<double>(v.max)/1e9)));v.step=1;
        }
        if(v.min>v.max||v.step<1||v.max>LONG_MAX||v.min<LONG_MIN)throw HRESULT_FROM_WIN32(ERROR_INVALID_DATA);
        if(p.Flags==KSPROPERTY_TYPE_BASICSUPPORT||p.Flags==KSPROPERTY_TYPE_DEFAULTVALUES){
            LONG initial=0;
            if(id==property::Exposure)initial=-6;
            if(id==property::ISO||id==property::Gamma||id==property::Contrast)initial=100;
            if(id==property::PowerLine)initial=3;
            if(id==property::Temperature)initial=6500;
            support(p,v,initial,data,size,returned);return;
        }
        KSPROPERTY_CAMERACONTROL_S output{};output.Property=p;
        *returned=sizeof(output);if(!data||size<sizeof(output))throw HRESULT_FROM_WIN32(ERROR_MORE_DATA);
        if(p.Flags==KSPROPERTY_TYPE_SET){
            std::memcpy(&output,data,sizeof(output));
            if(output.Flags!=KSPROPERTY_CAMERACONTROL_FLAGS_AUTO&&output.Flags!=KSPROPERTY_CAMERACONTROL_FLAGS_MANUAL)throw E_INVALIDARG;
            uint32_t mode=output.Flags;
            if(mode==property::Auto&&id==property::Focus&&(native.flags>>8&property::Continuous))mode=property::Continuous;
            if(mode==property::Manual&&(output.Value<v.min||output.Value>v.max||(output.Value-v.min)%v.step))throw E_INVALIDARG;
            const auto number=id==property::Exposure&&mode==property::Manual?exposureNS(output.Value):static_cast<int64_t>(output.Value);
            v=exchange(property::Set,id,sensor,number,mode);
        }else{v=native;}
        output.Property=p;
        output.Capabilities=(v.flags>>8)&(property::Auto|property::Manual);
        if(v.flags>>8&property::Continuous)output.Capabilities|=KSPROPERTY_CAMERACONTROL_FLAGS_AUTO;
        output.Flags=v.flags&(property::Auto|property::Continuous)?KSPROPERTY_CAMERACONTROL_FLAGS_AUTO:KSPROPERTY_CAMERACONTROL_FLAGS_MANUAL;
        output.Value=id==property::Exposure?exposureStep(v.value>0?v.value:std::max(v.min,v.max/2)):static_cast<LONG>(v.value);
        copyResult(data,size,returned,&output,sizeof(output));
    });
        if(result==HRESULT_FROM_WIN32(ERROR_BUSY)||result==HRESULT_FROM_WIN32(ERROR_TIMEOUT)){
            char stage[128]{};
            if(request&&requestBytes>=sizeof(KSPROPERTY))
                std::snprintf(stage,sizeof(stage),"property set=%08lx id=%lu flags=%lu",static_cast<unsigned long>(request->Set.Data1),static_cast<unsigned long>(request->Id),static_cast<unsigned long>(request->Flags));
            else std::snprintf(stage,sizeof(stage),"property invalid request");
            cameraTrace(stage,sensor,result);
        }
        return result;
    }
};
}
