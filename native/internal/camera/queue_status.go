package camera

import "syscall"

// A joined EAGAIN + EIO/ownership failure is NOT backpressure. Every leaf must
// be a permitted status. Wrappers are accepted; cycles/oversized trees fail closed.
func onlyQueueStatus(err error, allow func(error) bool) bool {
	budget := 64
	var walk func(error, int) bool
	walk = func(e error, depth int) bool {
		budget--
		if e == nil || depth > 16 || budget < 0 {
			return false
		}
		if allow(e) {
			return true
		}
		if multiple, ok := e.(interface{ Unwrap() []error }); ok {
			children := multiple.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !walk(child, depth+1) {
					return false
				}
			}
			return true
		}
		if single, ok := e.(interface{ Unwrap() error }); ok {
			return walk(single.Unwrap(), depth+1)
		}
		return false
	}
	return walk(err, 0)
}

func wouldBlock(err error) bool {
	return onlyQueueStatus(err, func(e error) bool { return e == syscall.EAGAIN || e == syscall.EINTR })
}
