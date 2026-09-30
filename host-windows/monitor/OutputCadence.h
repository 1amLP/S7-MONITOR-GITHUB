#pragma once
#include <cstdint>
#include <limits>
namespace s7 {
// Exact 60 Hz average, bounded catch-up, independent of the capture refresh.
class OutputCadence {
    uint64_t due_=0;
    unsigned remainder_=0;
public:
    void reset()noexcept{due_=0;remainder_=0;}
    bool ready(uint64_t now)const noexcept{return !due_||now>=due_;}
    uint64_t waitUS(uint64_t now)const noexcept{return due_>now?due_-now:0;}
    void submitted(uint64_t now)noexcept{
        if(!due_||(now>=due_&&now-due_>=16667)){due_=now;remainder_=0;}
        uint64_t step=16666;remainder_+=40;
        if(remainder_>=60){++step;remainder_-=60;}
        due_=due_>std::numeric_limits<uint64_t>::max()-step?std::numeric_limits<uint64_t>::max():due_+step;
    }
};
}
