// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once

namespace s7camera {
enum class ReaderCloseAction { None, Flush, ReleaseTerminal };
inline constexpr ReaderCloseAction readerCloseAction(bool hasReader,bool terminalError) noexcept {
    if(!hasReader)return ReaderCloseAction::None;
    return terminalError?ReaderCloseAction::ReleaseTerminal:ReaderCloseAction::Flush;
}
}
