// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once
#include <memory>
#include <mutex>
namespace s7 {
// One physical target, one transport at a time. An owner can be retired only
// after every kernel reference is gone. This class is shared by production and
// host tests; it does not simulate successful Windows I/O.
template<class T> class IoLease {
    std::mutex mutex_;
    std::shared_ptr<T> owner_;
    void collectLocked(){if(owner_ && owner_->releasable())owner_.reset();}
public:
    bool available(){std::lock_guard<std::mutex> lock(mutex_);collectLocked();return !owner_;}
    bool acquire(const std::shared_ptr<T>& next){
        std::lock_guard<std::mutex> lock(mutex_);collectLocked();
        if(owner_)return false;
        owner_=next;return true;
    }
    void collect(){std::lock_guard<std::mutex> lock(mutex_);collectLocked();}
};
}
