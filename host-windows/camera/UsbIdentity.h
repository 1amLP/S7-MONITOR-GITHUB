#pragma once
#include "../UsbIdentity.h"
namespace s7camera {
// Select the composite product, not a fixed MI_xx. Adding audio/camera changes
// interface numbers. VID/PID is only a first filter: require serial + ContainerId.
inline bool s7UsbFunction(std::wstring_view input){
    return s7identity::function(input);
}
}
