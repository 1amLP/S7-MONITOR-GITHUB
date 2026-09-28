#pragma once
// S7M1 v2 / S7C1 v2. Portable production wire code; no Windows dependency.
#include <array>
#include <cstdint>
#include <cstring>
#include <stdexcept>
#include <vector>
#include <limits>
namespace s7 {
using Bytes = std::vector<uint8_t>;
constexpr size_t FrameHeaderBytes=48, ConfigBytes=48, MaxAccessUnit=1024*1024;
inline uint16_t le16(const uint8_t* p){return uint16_t(p[0])|(uint16_t(p[1])<<8);}
inline uint32_t le32(const uint8_t* p){return uint32_t(p[0])|(uint32_t(p[1])<<8)|(uint32_t(p[2])<<16)|(uint32_t(p[3])<<24);}
inline void put16(uint8_t* p,uint16_t x){for(unsigned i=0;i<2;i++)p[i]=uint8_t(x>>(8*i));}
inline void put32(uint8_t* p,uint32_t x){for(unsigned i=0;i<4;i++)p[i]=uint8_t(x>>(8*i));}
inline void put64(uint8_t* p,uint64_t x){for(unsigned i=0;i<8;i++)p[i]=uint8_t(x>>(8*i));}
struct Config {
    uint32_t generation=0,fps=60,bitrate=12000000,keyRequest=0,gopSeconds=1;
	uint32_t width=1280,height=720;
	static bool mode(uint32_t w,uint32_t h){return (w==1280&&h==720)||(w==2560&&h==1440);}
    bool enabled=false,consumer=false;
    bool sniper=false;
    static Config parse(const uint8_t* p,size_t size){
        if(!p||size!=ConfigBytes||std::memcmp(p,"S7C1",4)||
           (le16(p+4)!=2&&le16(p+4)!=3)||(le16(p+4)==2&&le16(p+6)>3)||
           !mode(le32(p+12),le32(p+16)))throw std::runtime_error("Invalid S7C1 configuration");
        Config c;c.generation=le32(p+8);c.fps=le32(p+20);c.bitrate=le32(p+24);c.keyRequest=le32(p+28);c.gopSeconds=le32(p+44);
		c.width=le32(p+12);c.height=le32(p+16);
        c.enabled=(le16(p+6)&1)!=0;c.consumer=(le16(p+6)&2)!=0;
        c.sniper=le16(p+4)==3;
        if(c.sniper&&(!(le16(p+6)&4)||(le16(p+6)>>4)<100||(le16(p+6)>>4)>1600))throw std::runtime_error("Invalid Sniper configuration");
        if((c.gopSeconds!=1&&c.gopSeconds!=2&&c.gopSeconds!=5)||!c.generation||c.fps!=60||c.bitrate<2000000||c.bitrate>30000000)throw std::runtime_error("Unsupported S7 monitor mode");
        return c;
    }
};
inline std::array<uint8_t,FrameHeaderBytes> frameHeader(const Config& c,size_t bytes,uint64_t sequence,uint64_t pts,bool idr){
    if(bytes<4||bytes>MaxAccessUnit||!sequence||sequence>uint64_t(INT64_MAX)||pts>uint64_t(INT64_MAX)||!c.generation||c.fps!=60||!Config::mode(c.width,c.height))throw std::runtime_error("Invalid S7 frame bounds");
    std::array<uint8_t,FrameHeaderBytes> p{};std::memcpy(p.data(),"S7M1",4);put16(p.data()+4,2);put16(p.data()+6,idr?1:0);
    put32(p.data()+8,c.width);put32(p.data()+12,c.height);put32(p.data()+16,uint32_t(bytes));put32(p.data()+20,c.fps);
    put64(p.data()+24,sequence);put64(p.data()+32,pts);put32(p.data()+40,c.generation);return p;
}
// Converts one complete encoder sample. Transport fragmentation is a separate layer.
class AnnexB {
    enum class Framing {Unknown,AnnexB,Avcc};
    Bytes sps_,pps_;
    Framing framing_=Framing::Unknown,preferred_=Framing::Unknown;
    static void append(Bytes& out,const uint8_t* p,size_t n){
        if(!n||(p[0]&0x80)||(p[0]&31)==0||(p[0]&31)>=24)throw std::runtime_error("Invalid H.264 NAL header");
        if(n>MaxAccessUnit||out.size()+4+n>MaxAccessUnit)throw std::runtime_error("H.264 access unit exceeds 1 MiB");
        out.insert(out.end(),{0,0,0,1});out.insert(out.end(),p,p+n);
    }
    static size_t start(const uint8_t* p,size_t n,size_t i){
        if(i+3<=n&&p[i]==0&&p[i+1]==0&&p[i+2]==1)return 3;
        if(i+4<=n&&p[i]==0&&p[i+1]==0&&p[i+2]==0&&p[i+3]==1)return 4;
        return 0;
    }
    static Bytes fromAnnexB(const uint8_t* p,size_t n){
        if(!start(p,n,0))throw std::runtime_error("Missing Annex B start code");
        Bytes out;
        size_t i=0;
        while(i<n){
            size_t len=start(p,n,i);if(!len)throw std::runtime_error("Invalid Annex B boundary");
            size_t begin=i+len,j=begin;while(j<n&&!start(p,n,j))j++;
            size_t end=j;while(end>begin&&p[end-1]==0)end--;append(out,p+begin,end-begin);i=j;
        }
        return out;
    }
    static Bytes fromAvcc(const uint8_t* p,size_t n){
        Bytes out;
        size_t i=0;
        while(i<n){
            if(n-i<4)throw std::runtime_error("Truncated AVC length");
            uint32_t len=(uint32_t(p[i])<<24)|(uint32_t(p[i+1])<<16)|(uint32_t(p[i+2])<<8)|p[i+3];i+=4;
            if(!len||len>n-i)throw std::runtime_error("Invalid AVC length");
            append(out,p+i,len);i+=len;
        }
        return out;
    }
    static bool tryParse(Bytes& out,const uint8_t* p,size_t n,Framing format){
        try{out=format==Framing::Avcc?fromAvcc(p,n):fromAnnexB(p,n);return true;}
        catch(const std::runtime_error&){return false;}
    }
    Bytes normalize(const uint8_t* p,size_t n){
        if(!p||n<4||n>MaxAccessUnit)throw std::runtime_error("H.264 sample size invalid");
        if(!start(p,n,0)){Bytes out=fromAvcc(p,n);framing_=Framing::Avcc;return out;}
        // An AVCC length can begin with an Annex B start code. Check both complete parses.
        Bytes avcc,annex;
        const bool avccValid=tryParse(avcc,p,n,Framing::Avcc);
        const bool annexValid=tryParse(annex,p,n,Framing::AnnexB);
        if(avccValid&&annexValid){
            if(avcc==annex)return avcc;
            if(framing_==Framing::Unknown)framing_=preferred_;
            if(framing_==Framing::Unknown)throw std::runtime_error("Ambiguous H.264 sample framing");
            return framing_==Framing::Avcc?avcc:annex;
        }
        if(avccValid){framing_=Framing::Avcc;return avcc;}
        if(annexValid){framing_=Framing::AnnexB;return annex;}
        throw std::runtime_error("Invalid H.264 sample framing");
    }
    void remember(const Bytes& b){
        for(size_t i=0;i<b.size();){
            size_t begin=i+4,j=begin;while(j<b.size()&&!start(b.data(),b.size(),j))j++;
            unsigned type=b[begin]&31;
            if(type==7)sps_=Bytes(b.begin()+i,b.begin()+j);
            if(type==8)pps_=Bytes(b.begin()+i,b.begin()+j);
            i=j;
        }
    }
public:
    void reset(){sps_.clear();pps_.clear();framing_=preferred_=Framing::Unknown;}
    void config(const uint8_t* p,size_t n){
        if(!p||!n)return;
        if(p[0]==1){
            if(n<7||(p[4]&3)!=3)throw std::runtime_error("Unsupported AVC configuration record");
            Bytes b;size_t i=6;unsigned count=p[5]&31;
            for(unsigned pass=0;pass<2;pass++){
                if(pass){if(i>=n)throw std::runtime_error("Missing PPS count");count=p[i++];}
                for(unsigned k=0;k<count;k++){
                    if(i+2>n)throw std::runtime_error("Truncated AVC config");
                    size_t len=(size_t(p[i])<<8)|p[i+1];i+=2;
                    if(!len||len>n-i)throw std::runtime_error("Invalid parameter set length");
                    append(b,p+i,len);i+=len;
                }
            }
            remember(b);
            framing_=Framing::Unknown;preferred_=Framing::Avcc;
        }else{
            if(n<4||n>MaxAccessUnit)throw std::runtime_error("H.264 configuration size invalid");
            remember(fromAnnexB(p,n));
            framing_=preferred_=Framing::Unknown;
        }
    }
    Bytes sample(const uint8_t* p,size_t n,bool& idr){
        Bytes b=normalize(p,n);remember(b);idr=false;bool slice=false,hasSPS=false,hasPPS=false;
        for(size_t i=0;i<b.size();){
            size_t begin=i+4,j=begin;while(j<b.size()&&!start(b.data(),b.size(),j))j++;
            unsigned type=b[begin]&31;idr|=type==5;slice|=type==1||type==5;hasSPS|=type==7;hasPPS|=type==8;i=j;
        }
        if(!slice)throw std::runtime_error("Encoder sample has no picture");
        if(idr&&(!hasSPS||!hasPPS)){
            if(sps_.empty()||pps_.empty())throw std::runtime_error("IDR without SPS/PPS");
            Bytes prefix;if(!hasSPS)prefix.insert(prefix.end(),sps_.begin(),sps_.end());
            if(!hasPPS)prefix.insert(prefix.end(),pps_.begin(),pps_.end());
            prefix.insert(prefix.end(),b.begin(),b.end());b.swap(prefix);
        }
        if(b.size()>MaxAccessUnit)throw std::runtime_error("Prefixed AVC frame exceeds 1 MiB");
        return b;
    }
};
// BT.709 limited-range NV12. CPU conversion on PC, not on phone. Alpha ignored.
inline void bgraToNV12(const uint8_t* source,size_t pitch,uint8_t* destination,unsigned w,unsigned h){
    if(!source||!destination||!w||!h||(w&1)||(h&1)||pitch<size_t(w)*4||size_t(h)>SIZE_MAX/pitch)
        throw std::invalid_argument("NV12 geometry");
    auto clamp=[](int x){return uint8_t(x<0?0:x>255?255:x);};
    for(unsigned y=0;y<h;y+=2)for(unsigned x=0;x<w;x+=2){
        int r=0,g=0,b=0;
        for(unsigned dy=0;dy<2;dy++)for(unsigned dx=0;dx<2;dx++){
            auto p=source+size_t(y+dy)*pitch+4*(x+dx);int rr=p[2],gg=p[1],bb=p[0];r+=rr;g+=gg;b+=bb;
            destination[size_t(y+dy)*w+x+dx]=clamp(((11966*rr+40254*gg+4064*bb+32768)>>16)+16);
        }
        r=(r+2)/4;g=(g+2)/4;b=(b+2)/4;size_t uv=size_t(w)*h+(y/2)*size_t(w)+x;
        destination[uv]=clamp(((-6596*r-22189*g+28785*b+32768)>>16)+128);
        destination[uv+1]=clamp(((28785*r-26145*g-2640*b+32768)>>16)+128);
    }
}
}
