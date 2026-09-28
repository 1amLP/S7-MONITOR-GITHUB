#pragma once
#include <cstdint>
#include <cstddef>

namespace s7 {
enum class EncodedSize { Empty, Picture, Invalid };
constexpr EncodedSize encodedSize(std::size_t bytes,std::size_t maximum){
    return bytes==0?EncodedSize::Empty:(bytes<4||bytes>maximum?EncodedSize::Invalid:EncodedSize::Picture);
}

// Local codec faults may reopen the codec, not unplug the display or restart USB.
class EncoderRetryBudget {
    uint64_t start_=0;
    unsigned used_=0;
public:
    constexpr bool take(uint64_t now){
        if(!used_||now<start_||now-start_>=60000000){start_=now;used_=0;}
        if(used_==3)return false;
        ++used_;return true;
    }
};
constexpr bool encoderRecoveryContract(){
    EncoderRetryBudget b;
    return b.take(1)&&b.take(2)&&b.take(3)&&!b.take(4)&&!b.take(60000000)&&b.take(60000001);
}
static_assert(encoderRecoveryContract());
static_assert(encodedSize(0,1024)==EncodedSize::Empty);
static_assert(encodedSize(3,1024)==EncodedSize::Invalid);
static_assert(encodedSize(4,1024)==EncodedSize::Picture);
static_assert(encodedSize(1024,1024)==EncodedSize::Picture);
static_assert(encodedSize(1025,1024)==EncodedSize::Invalid);
}
