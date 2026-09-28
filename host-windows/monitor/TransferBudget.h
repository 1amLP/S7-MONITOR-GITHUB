// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once
#include <cstdint>
namespace s7 {
// One deadline for the complete framed access unit, not a new 600ms allowance
// for every 16KiB USB chunk. This limits delivery attempts, NOT physical latency.
class TransferBudget {
    uint64_t start_, duration_;
public:
    // A blocked access unit must not exceed the end-to-end p95 latency target.
    static constexpr uint64_t FrameUS=75000;
    explicit TransferBudget(uint64_t start,uint64_t duration=FrameUS):start_(start),duration_(duration){}
    uint32_t remainingMS(uint64_t now)const noexcept {
        if(!duration_||duration_>FrameUS||now<start_||now-start_>=duration_)return 0;
        return static_cast<uint32_t>((duration_-(now-start_)+999)/1000);
    }
};
}
