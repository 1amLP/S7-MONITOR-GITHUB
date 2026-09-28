// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once
#include <cstdint>
#include <limits>
#include <stdexcept>

namespace s7camera {
// All timestamps are 100 ns. The source and arrival clocks are different domains.
// Anchor once; preserve source deltas, including gaps. Never manufacture FPS by
// replacing a repeated timestamp with arrival time or previous+1.
class CaptureTimeline {
public:
    struct Sample {int64_t time=0,duration=0;bool discontinuity=false;};
    struct Stats {
        uint64_t observed=0,delivered=0,withoutRequest=0,invalid=0,gaps=0;
        int64_t firstSource=0,lastSource=0,firstArrival=0,lastArrival=0;
    };
private:
    int64_t duration_,origin_=0;
    bool pendingGap_=true,haveSample_=false;
    Sample latest_{};
    Stats stats_{};
public:
    CaptureTimeline(uint32_t rate,uint32_t denominator){
        if(!rate||!denominator||rate>240||denominator!=1)
            throw std::invalid_argument("unsupported camera clock rate");
        duration_=10000000LL*denominator/rate;
    }
    void gap() noexcept {pendingGap_=true;}
    void setRate(uint32_t rate,uint32_t denominator){
        if(!rate||rate>240||denominator!=1||haveSample_)throw std::invalid_argument("camera rate change");
        duration_=10000000LL/rate;pendingGap_=true;
    }
    void observe(int64_t source,int64_t arrival,bool sourceGap=false){
        if(haveSample_)throw std::logic_error("unconsumed camera sample");
        auto invalid=[&](){++stats_.invalid;throw std::runtime_error("camera source timestamp is invalid or nonmonotonic");};
        if(source<0||arrival<0||(stats_.observed&&source<=stats_.lastSource))invalid();
        if(stats_.observed&&arrival<stats_.lastArrival)invalid();
        // Overflow-safe, bounded source/arrival deltas. A new session is needed
        // after a stalled/reset source; do not conceal it with a new clock anchor.
        if(stats_.observed&&source-stats_.lastSource>50000000LL)invalid();
        const int64_t first=stats_.observed?stats_.firstSource:source;
        const int64_t base=stats_.observed?origin_:arrival;
        const int64_t delta=source-first;
        if(delta>std::numeric_limits<int64_t>::max()-base)invalid();
        const bool gapDetected=stats_.observed&&source-stats_.lastSource>duration_+duration_/2;
        if(gapDetected||sourceGap){pendingGap_=true;++stats_.gaps;}
        if(!stats_.observed){stats_.firstSource=source;stats_.firstArrival=arrival;origin_=arrival;}
        stats_.lastSource=source;stats_.lastArrival=arrival;++stats_.observed;
        latest_={base+delta,duration_,pendingGap_};haveSample_=true;
    }
    // Every observed sample is either delivered once or deliberately skipped.
    Sample pending()const{
        if(!haveSample_)throw std::logic_error("no fresh camera sample");
        return latest_;
    }
    Sample deliver(){
        if(!haveSample_)throw std::logic_error("no fresh camera sample");
        haveSample_=false;pendingGap_=false;++stats_.delivered;return latest_;
    }
    void noRequest(){
        if(!haveSample_)throw std::logic_error("no camera sample to skip");
        haveSample_=false;pendingGap_=true;++stats_.withoutRequest;
    }
    Stats stats()const noexcept{return stats_;}
};
}
