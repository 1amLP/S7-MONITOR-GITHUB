// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once
#include <memory>
#include <mutex>

namespace s7camera {
// One source reader per DLL process. A timed-out async flush is NOT completion.
// Retain at most one complete resource owner, block retries, and pin the DLL.
// Poison has no time-based reset. An actual late barrier, or a terminal reader
// whose COM ownership was released without another method call, may be retired
// only after completed physical cleanup on a COM-initialized caller.
// Phone-side selection still arbitrates owners across separate processes.
template<class T> class ReaderLifetime {
    mutable std::mutex mutex_;
    std::shared_ptr<T> owner_;
    bool poisoned_=false,recovering_=false;
public:
    bool acquire(std::shared_ptr<T> const& owner){
        if(!owner)return false;
        std::lock_guard<std::mutex> lock(mutex_);
        if(owner_)return false;
        owner_=owner;return true;
    }
    bool finish(std::shared_ptr<T> const& owner,bool complete){
        std::shared_ptr<T> released;
        {
            std::lock_guard<std::mutex> lock(mutex_);
            if(!owner||owner_!=owner||recovering_)return false;
            if(!complete)poisoned_=true;
            if(poisoned_)return false;
            released=std::move(owner_);
        }
        // Resource destructors run without the registry lock held.
        released.reset();return true;
    }
    // Only an actual completion barrier may make an old owner reclaimable.
    // Keep the slot occupied through slow cleanup and run it without this lock.
    template<class Ready,class Finish> bool recover(Ready&& ready,Finish&& finish){
        std::shared_ptr<T> candidate;
        {
            std::lock_guard<std::mutex> lock(mutex_);
            if(!owner_||!poisoned_||recovering_||!ready(*owner_))return false;
            recovering_=true;candidate=owner_;
        }
        bool complete=false;
        try{complete=finish(*candidate);}catch(...){
            std::lock_guard<std::mutex> lock(mutex_);recovering_=false;throw;
        }
        {
            std::lock_guard<std::mutex> lock(mutex_);
            recovering_=false;
            if(complete){owner_.reset();poisoned_=false;}
        }
        candidate.reset();return complete;
    }
    bool busy()const{std::lock_guard<std::mutex> lock(mutex_);return bool(owner_);}
    // Signal the current owner; never release its slot or resources here.
    // A switch can Start the new virtual endpoint before Stop on the old one.
    template<class F> bool requestHandoff(F&& signal){
        std::lock_guard<std::mutex> lock(mutex_);
        return owner_&&!poisoned_&&!recovering_&&signal(*owner_);
    }
    bool poisoned()const{std::lock_guard<std::mutex> lock(mutex_);return poisoned_;}
    template<class F> bool retains(F&& predicate)const{
        std::lock_guard<std::mutex> lock(mutex_);
        return poisoned_&&owner_&&predicate(*owner_);
    }
};
}
