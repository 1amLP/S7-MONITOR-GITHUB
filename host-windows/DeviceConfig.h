#pragma once

#ifndef S7_TARGET_SERIAL
#define S7_TARGET_SERIAL L""
#endif

namespace s7 {
inline constexpr wchar_t TargetSerial[] = S7_TARGET_SERIAL;
}
