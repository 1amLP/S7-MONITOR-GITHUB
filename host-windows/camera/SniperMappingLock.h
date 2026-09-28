#pragma once
#include <mutex>
namespace s7 {
inline std::mutex& sniperMappingMutex(){static std::mutex mutex;return mutex;}
}
