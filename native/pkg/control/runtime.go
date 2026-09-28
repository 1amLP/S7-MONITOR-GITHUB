package control

import (
	"errors"
	"sync"
)

// Runtime survives USB sessions; manual commands are serialized with persistence.
type Runtime struct {
	mu                         sync.Mutex
	commandMu                  sync.Mutex
	wanted, blocked, connected bool
	state, lastError           string
	epoch                      uint64
	status                     func() Response
	profile                    func(string) error
	cancel                     func()
	wake                       chan struct{}
	persist                    func(bool) error
}

func NewRuntime(wanted bool, persist func(bool) error) *Runtime {
	state := "user_disabled"
	if wanted {
		state = "waiting_usb"
	}
	return &Runtime{wanted: wanted, state: state, wake: make(chan struct{}, 1), persist: persist}
}
func (r *Runtime) Wake() <-chan struct{} { return r.wake }
func (r *Runtime) Wanted() bool          { r.mu.Lock(); defer r.mu.Unlock(); return r.wanted && !r.blocked }
func (r *Runtime) Attach(cancel func(), status func() Response, profile func(string) error) {
	r.mu.Lock()
	r.epoch++
	r.cancel = cancel
	r.status = status
	r.profile = profile
	stop := !r.wanted || r.blocked
	if !stop {
		r.state = "connecting"
		r.lastError = ""
	}
	r.mu.Unlock()
	// A disable command can race the setup between preflight and Attach.
	// Never publish an uncancellable new session after that command.
	if stop && cancel != nil {
		cancel()
	}
}

// Waiting is an observable non-error state. It does not change saved intent.
func (r *Runtime) Waiting(state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wanted && !r.blocked {
		r.connected = false
		r.state = state
	}
}

// USBState describes actual FunctionFS enable/suspend state, not just UDC bind.
func (r *Runtime) USBState(connected bool, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wanted && !r.blocked {
		r.connected = connected
		r.state = state
	}
}
func (r *Runtime) Connected() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wanted && !r.blocked {
		r.connected = true
		r.state = "connected"
	}
}
func (r *Runtime) Detach() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.epoch++
	r.status = nil
	r.profile = nil
	r.cancel = nil
	r.connected = false
	if !r.blocked {
		if r.wanted {
			r.state = "waiting_usb"
		} else {
			r.state = "user_disabled"
		}
	}
}
func (r *Runtime) Fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blocked = true
	r.connected = false
	r.state = "error"
	r.lastError = err.Error()
	if len(r.lastError) > 256 {
		r.lastError = r.lastError[:256]
	}
}
func (r *Runtime) Status() Response {
	r.mu.Lock()
	f, epoch := r.status, r.epoch
	r.mu.Unlock()
	var result Response
	if f != nil {
		result = f()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if epoch != r.epoch {
		result = Response{}
	}
	result.MonitorWanted = r.wanted
	result.MonitorControl = true
	result.MonitorConnected = r.connected
	result.MonitorState = r.state
	result.MonitorError = r.lastError
	return result
}
func (r *Runtime) Profile(value string) error {
	r.mu.Lock()
	f, connected := r.profile, r.connected
	r.mu.Unlock()
	if !connected || f == nil {
		return errors.New("camera session unavailable")
	}
	return f(value)
}
func (r *Runtime) Monitor(command string) error {
	r.commandMu.Lock()
	defer r.commandMu.Unlock()
	if command == "usb_toggle" {
		r.mu.Lock()
		wanted := r.wanted
		r.mu.Unlock()
		if wanted {
			command = "monitor_disconnect"
		} else {
			command = "monitor_connect"
		}
	}
	if command != "monitor_connect" && command != "monitor_disconnect" && command != "monitor_reconnect" {
		return errors.New("invalid monitor command")
	}
	wanted := command != "monitor_disconnect"
	if r.persist != nil {
		if err := r.persist(wanted); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.wanted = wanted
	r.blocked = false
	r.lastError = ""
	cancel := r.cancel
	if command == "monitor_connect" && cancel != nil {
		cancel = nil
	}
	if cancel != nil {
		r.state = "disconnecting"
	} else if !wanted {
		r.connected = false
		r.state = "user_disabled"
	} else if r.cancel == nil {
		r.state = "waiting_usb"
	}
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return nil
}
