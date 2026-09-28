#pragma once
// Portable arithmetic only. Passing these tests does not test an S7 or Windows.
#include <algorithm>
#include <cmath>
#include <cstdint>
#include <vector>
namespace s7probe {
struct FpsMetrics {
    uint64_t frames=0, nonmonotonic=0, estimatedMissing=0;
    int64_t firstPTS=0, lastPTS=0;
    double firstArrival=0, lastArrival=0;
    std::vector<double> gaps;
    void add(int64_t pts,double arrival,unsigned fps) {
        if(!std::isfinite(arrival)||!fps){++nonmonotonic;return;}
        if(frames){
            const long double dt=static_cast<long double>(pts)-lastPTS;
            if(dt<=0||arrival<lastArrival)++nonmonotonic;
            if(dt>0){const long double intervals=dt*fps/10000000.0L;
                if(intervals>1.5L && intervals<1e9L)estimatedMissing+=static_cast<uint64_t>(std::llround(intervals)-1);}
            gaps.push_back(std::max(0.0,arrival-lastArrival)*1000.0);
        }else{firstPTS=pts;firstArrival=arrival;}
        ++frames;lastPTS=pts;lastArrival=arrival;
    }
    double wallFPS()const{return frames>1&&lastArrival>firstArrival?double(frames-1)/(lastArrival-firstArrival):0;}
    double ptsFPS()const{return frames>1&&lastPTS>firstPTS?static_cast<double>((frames-1)*10000000.0L/(static_cast<long double>(lastPTS)-firstPTS)):0;}
    double percentile(double q)const{if(gaps.empty()||!std::isfinite(q))return 0;auto v=gaps;std::sort(v.begin(),v.end());const auto rank=static_cast<size_t>(std::ceil(std::clamp(q,0.0,1.0)*v.size()));return v[std::min(v.size()-1,std::max(size_t(1),rank)-1)];}
    bool rateObserved(unsigned fps,double seconds)const{
        if(!fps||!std::isfinite(seconds)||seconds<=0)return false;
        return frames>=fps*seconds*.95 && lastArrival-firstArrival>=seconds-.25 &&
            !nonmonotonic && estimatedMissing<=frames/100 && wallFPS()>=fps*.98 && wallFPS()<=fps*1.02 &&
            ptsFPS()>=fps*.98 && ptsFPS()<=fps*1.02 && percentile(1)<=std::max(50.0,3000.0/fps);
    }
};
}
