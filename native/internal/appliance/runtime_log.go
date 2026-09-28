package appliance

import (
	"io"
	"sync"
)

const runtimeLogLimit = 16 << 10

type runtimeLogBuffer struct {
	sync.Mutex
	data []byte
}

var runtimeMessages runtimeLogBuffer

func (b *runtimeLogBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.Lock()
	defer b.Unlock()
	if len(p) > runtimeLogLimit {
		p = p[len(p)-runtimeLogLimit:]
	}
	if cap(b.data) < runtimeLogLimit {
		b.data = make([]byte, 0, runtimeLogLimit)
	}
	if drop := len(b.data) + len(p) - runtimeLogLimit; drop > 0 {
		copy(b.data, b.data[drop:])
		b.data = b.data[:len(b.data)-drop]
	}
	b.data = append(b.data, p...)
	return n, nil
}

// Kernel warnings may overwrite printk before a host requests diagnostics.
// Keep native state transitions separately in bounded RAM, never a disk file.
func RuntimeLogWriter() io.Writer { return &runtimeMessages }
func RuntimeMessages() string {
	runtimeMessages.Lock()
	defer runtimeMessages.Unlock()
	return string(runtimeMessages.data)
}
