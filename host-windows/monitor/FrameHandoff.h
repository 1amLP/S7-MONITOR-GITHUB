// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once
#include <cstdint>
#include <cstddef>
#include <mutex>
#include <memory>
#include <utility>
#include <vector>

namespace s7 {
struct GpuFrame;
// Only uncompressed, independent NV12 pictures may be coalesced here. Encoded
// H.264 access units must stay ordered: dropping a P frame breaks its references.
struct StreamKey {
    uint64_t epoch=0;
    uint32_t generation=0, fps=0, bitrate=0, gop=0;
	uint32_t width=1280,height=720;
    bool valid()const noexcept {
        return epoch&&generation&&fps==60&&bitrate>=2000000&&bitrate<=30000000&&
               (gop==1||gop==2||gop==5)&&((width==1280&&height==720)||(width==2560&&height==1440));
    }
    bool operator==(const StreamKey& b)const noexcept {
        return epoch==b.epoch&&generation==b.generation&&fps==b.fps&&bitrate==b.bitrate&&gop==b.gop&&width==b.width&&height==b.height;
    }
    bool operator!=(const StreamKey& b)const noexcept{return !(*this==b);}
};
struct DesktopPicture {
    StreamKey key{};
    uint64_t acquiredUS=0;
    std::vector<uint8_t> pixels;
    std::shared_ptr<GpuFrame> gpu;
};
struct HandoffStats {
    uint64_t published=0, taken=0, replaced=0, stale=0, refreshed=0, rejected=0, cleared=0;
    unsigned pending=0;
};
class FrameHandoff {
    std::mutex mutex_;
    StreamKey key_{};
    DesktopPicture pending_[2]{};
    HandoffStats stats_{};
    uint64_t newestUS_=0,submittedUS_=0;
    bool active_=false, closed_=false;
    unsigned capacity_=1,head_=0;
    void discardFrontLocked(){
        if(!stats_.pending)return;
        auto& picture=pending_[head_];picture.acquiredUS=0;picture.gpu.reset();
        head_=(head_+1)%capacity_;--stats_.pending;++stats_.cleared;
    }
    void clearLocked(){
        while(stats_.pending)discardFrontLocked();
        head_=0;
    }
public:
    static constexpr std::size_t PictureBytes=1280*720*3/2;
    // At 60 Hz, a queued desktop picture may be at most two frame periods old.
    static constexpr uint64_t MaxAgeUS=33334;
    // One latest picture for Sniper; two ordered pictures for monitor resampling.
    // Mutex is held only for metadata and vector swaps, never allocation or I/O.
    bool configure(StreamKey key,unsigned capacity=1){
        std::lock_guard<std::mutex> lock(mutex_);
        if(closed_||!key.valid()||capacity<1||capacity>2)return false;
        if(!active_||key_!=key||capacity_!=capacity){clearLocked();capacity_=capacity;newestUS_=submittedUS_=0;key_=key;active_=true;}
        return true;
    }
    void pause(){std::lock_guard<std::mutex> lock(mutex_);clearLocked();active_=false;newestUS_=submittedUS_=0;}
    void close(){std::lock_guard<std::mutex> lock(mutex_);clearLocked();active_=false;closed_=true;}
    bool publish(DesktopPicture& picture){
        std::lock_guard<std::mutex> lock(mutex_);
        if(closed_||!active_||picture.key!=key_||
           (picture.gpu?!picture.pixels.empty():picture.pixels.size()!=size_t(key_.width)*key_.height*3/2)||
           !picture.acquiredUS||picture.acquiredUS<=newestUS_){++stats_.rejected;return false;}
        unsigned slot=(head_+stats_.pending)%capacity_;
        if(stats_.pending==capacity_){slot=head_;head_=(head_+1)%capacity_;++stats_.replaced;}
        else ++stats_.pending;
        newestUS_=picture.acquiredUS;
        std::swap(pending_[slot],picture);++stats_.published;
        return true;
    }
    bool take(StreamKey key,uint64_t nowUS,DesktopPicture& picture,bool allowSnapshot=false){
        std::lock_guard<std::mutex> lock(mutex_);
        if(closed_||!active_||key!=key_||!stats_.pending)return false;
        while(stats_.pending){
            auto& next=pending_[head_];
            const bool old=nowUS>=next.acquiredUS&&nowUS-next.acquiredUS>MaxAgeUS;
            if(nowUS<next.acquiredUS||next.acquiredUS<=submittedUS_||(old&&(!allowSnapshot||stats_.pending>1))){
                ++stats_.stale;discardFrontLocked();continue;
            }
            // A cold MFT can take >100ms to open while the desktop is static.
            // Only the latest old snapshot may supply an initial/requested IDR.
            if(old)++stats_.refreshed;else ++stats_.taken;
            std::swap(next,picture);next.acquiredUS=0;next.gpu.reset();
            head_=(head_+1)%capacity_;--stats_.pending;
            return true;
        }
        return false;
    }
    void submitted(uint64_t pts){
        std::lock_guard<std::mutex> lock(mutex_);
        if(active_&&pts>submittedUS_)submittedUS_=pts;
    }
    HandoffStats stats(){std::lock_guard<std::mutex> lock(mutex_);return stats_;}
};
inline uint64_t samplePTS(bool fresh,uint64_t acquiredUS,uint64_t nowUS,uint64_t previousUS)noexcept{
    const uint64_t candidate=fresh?acquiredUS:nowUS;
    return candidate>previousUS?candidate:0;
}
}
