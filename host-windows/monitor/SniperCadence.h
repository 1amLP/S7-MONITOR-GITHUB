#pragma once
#include <cstdint>
#include <algorithm>
namespace s7 {
constexpr uint64_t sniperDeadline(uint64_t previous,uint64_t now){
    constexpr uint64_t period=1000000/60;
    return !previous||now<previous||now-previous>=period?now+period:previous+period;
}
constexpr uint64_t sniperWait(uint64_t next,uint64_t now,bool pending){
    const uint64_t maximum=pending?1000:5000;
    return next>now?std::max<uint64_t>(100,std::min(maximum,next-now)):maximum;
}
}
