#pragma once
#include <cstdint>
#include "EncoderRecovery.h"
namespace s7 {
constexpr bool sniperTransientExit(uint32_t result){
    return result==0||result==0x887a0026u||result==0x887a0028u||result==0x887a0005u||result==0x887a0007u;
}
class SniperRestart {
    uint32_t generation_=0;
    uint64_t retryAt_=0;
    bool blocked_=false;
    EncoderRetryBudget budget_;
public:
    bool ready(uint32_t generation,uint64_t now){
        if(generation!=generation_){generation_=generation;blocked_=false;retryAt_=0;budget_={};}
        return !blocked_&&now>=retryAt_;
    }
    bool exited(uint32_t result,uint64_t now){
        if(!sniperTransientExit(result)||!budget_.take(now)){blocked_=true;return false;}
        retryAt_=now+250000;return true;
    }
};
}
