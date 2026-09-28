// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once
#include <cstdint>
#include <stdexcept>
namespace s7 {
// Watcher-thread ownership only. A failed IddCxMonitorDeparture does not release
// the monitor, so forgetting the handle would allow a second live desktop.
// The parent WDF device remains the final framework owner during destruction.
template<class Handle> class MonitorSlot {
    Handle handle_{};
    uint32_t fps_=0;
    bool pending_=false;
public:
    Handle get()const noexcept{return handle_;}
    uint32_t fps()const noexcept{return fps_;}
    bool occupied()const noexcept{return handle_!=Handle{};}
    bool pending()const noexcept{return pending_;}
    void adopt(Handle handle,uint32_t fps){
        if(occupied()||handle==Handle{}||fps!=60)throw std::logic_error("Invalid or duplicate S7 monitor ownership");
        handle_=handle;fps_=fps;pending_=false;
    }
    Handle beginDeparture()noexcept{pending_=occupied();return handle_;}
    bool finishDeparture(Handle expected,bool success)noexcept{
        if(!occupied()||handle_!=expected||!pending_)return false;
        if(!success)return false;
        handle_=Handle{};fps_=0;pending_=false;return true;
    }
};
}
