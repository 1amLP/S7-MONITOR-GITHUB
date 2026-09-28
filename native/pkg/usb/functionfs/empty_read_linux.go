package functionfs

import (
	"os"
)

// An ordinary os.File.Read with an empty slice never reaches FunctionFS.
// RawConn keeps the descriptor alive during the syscall and preserves the
// nonblocking/poller behaviour. A queued kernel request still requires UDC
// shutdown to complete; closing the userspace handle alone is not cancellation.
func acknowledgeEmptyRead(file *os.File) error {
	_, err := controlIO(file, nil, true)
	return err
}
