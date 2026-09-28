// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once
#include <cstdint>
#include <cstddef>
#include <mutex>
#include <utility>
#include <vector>

namespace s7 {
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
};
struct HandoffStats {
    uint64_t published=0, taken=0, replaced=0, stale=0, refreshed=0, rejected=0, cleared=0;
    unsigned pending=0;
};
class FrameHandoff {
    std::mutex mutex_;
    StreamKey key_{};
    DesktopPicture pending_{};
    HandoffStats stats_{};
    uint64_t newestUS_=0,submittedUS_=0;
    bool active_=false, closed_=false;
    void clearLocked(){
        if(stats_.pending){++stats_.cleared;stats_.pending=0;}
        pending_.acquiredUS=0;
    }
public:
    static constexpr std::size_t PictureBytes=1280*720*3/2;
    // At 60 Hz, a queued desktop picture may be at most two frame periods old.
    static constexpr uint64_t MaxAgeUS=33334;
    // One pending picture; producer/consumer each own at most one other picture.
    // Mutex is held only for metadata and vector swaps, never allocation or I/O.
    bool configure(StreamKey key){
        std::lock_guard<std::mutex> lock(mutex_);
        if(closed_||!key.valid())return false;
        if(!active_||key_!=key){clearLocked();newestUS_=submittedUS_=0;key_=key;active_=true;}
        return true;
    }
    void pause(){std::lock_guard<std::mutex> lock(mutex_);clearLocked();active_=false;newestUS_=submittedUS_=0;}
    void close(){std::lock_guard<std::mutex> lock(mutex_);clearLocked();active_=false;closed_=true;}
    bool publish(DesktopPicture& picture){
        std::lock_guard<std::mutex> lock(mutex_);
        if(closed_||!active_||picture.key!=key_||picture.pixels.size()!=size_t(key_.width)*key_.height*3/2||
           !picture.acquiredUS||picture.acquiredUS<=newestUS_){++stats_.rejected;return false;}
        if(stats_.pending)++stats_.replaced;
        newestUS_=picture.acquiredUS;
        std::swap(pending_,picture);++stats_.published;stats_.pending=1;
        return true;
    }
    bool take(StreamKey key,uint64_t nowUS,DesktopPicture& picture,bool allowSnapshot=false){
        std::lock_guard<std::mutex> lock(mutex_);
        if(closed_||!active_||key!=key_||!stats_.pending)return false;
        const bool old=nowUS>=pending_.acquiredUS&&nowUS-pending_.acquiredUS>MaxAgeUS;
        if(nowUS<pending_.acquiredUS||pending_.acquiredUS<=submittedUS_||(old&&!allowSnapshot)){
            ++stats_.stale;clearLocked();return false;
        }
        // A cold MFT can take >100ms to open while the desktop is static.
        // For initial/requested IDR only, permit the latest snapshot without
        // changing its acquisition time or counting it as a fresh picture.
        if(old)++stats_.refreshed;else ++stats_.taken;
        std::swap(pending_,picture);stats_.pending=0;pending_.acquiredUS=0;
        return true;
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
