// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once
#include <cstdint>

namespace s7 {
class AcquireRetry {
    uint64_t activityUS_=0;
    bool active_=false;
public:
    void activity(uint64_t now) noexcept {activityUS_=now;active_=true;}
    uint64_t delayUS(bool streaming,uint64_t now) const noexcept {
        return streaming&&active_&&now>=activityUS_&&now-activityUS_<100000?2000:50000;
    }
};
}
