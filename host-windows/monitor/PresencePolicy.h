// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once
namespace s7 {
enum class Presence { Unknown, Present, Absent };
class PresencePolicy {
    unsigned missing_=0;
public:
    // Timeouts and enumeration errors are UNKNOWN, not physical unplug.
    // Require four consecutive successful enumerations without the target.
    bool observe(Presence value){
        if(value!=Presence::Absent){missing_=0;return false;}
        if(missing_<4)++missing_;
        return missing_==4;
    }
};
}
