//go:build linux

package functionfs

// Samsung 3.18 bulk endpoints do not support Go's netpoll readiness wait.
// Return EAGAIN to the bounded, cancellable protocol loop. Unlike Write, packet
// writes must reach the kernel for zero-length bulk IN termination too.
func (e *Endpoint) ReadPacket(b []byte) (int, error)  { return usbTransfer(e.file, b, true, false) }
func (e *Endpoint) WritePacket(b []byte) (int, error) { return usbTransfer(e.file, b, false, false) }
