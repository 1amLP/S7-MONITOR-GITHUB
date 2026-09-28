//go:build linux && (amd64 || arm64)

package sensorhub

import (
	"context"
	"errors"
	"fmt"
	"perimode/native/internal/childproc"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type Tail struct {
	mu    sync.Mutex
	data  []byte
	total uint64
}

func (t *Tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(b)
	t.total += uint64(n)
	const limit = 8192
	if n >= limit {
		t.data = append(t.data[:0], b[n-limit:]...)
	} else {
		if len(t.data)+n > limit {
			copy(t.data, t.data[len(t.data)+n-limit:])
			t.data = t.data[:limit-n]
		}
		t.data = append(t.data, b...)
	}
	return n, nil
}
func (t *Tail) Text() string { t.mu.Lock(); defer t.mu.Unlock(); return string(t.data) }

type commandProcess struct {
	cmd     *exec.Cmd
	done    chan struct{}
	tail    *Tail
	mu      sync.Mutex
	stopMu  sync.Mutex
	err     error
	cleanup func() error
	cleaned bool
}

func startCommand(ctx context.Context, path string, args, env []string, cleanup func() error) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return startConfiguredCommand(ctx, path, args, env, cleanup, &syscall.SysProcAttr{Setpgid: true})
}

func startConfiguredCommand(ctx context.Context, path string, args, env []string, cleanup func() error, attr *syscall.SysProcAttr, files ...*os.File) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c := exec.Command(path, args...)
	c.Env = env
	c.SysProcAttr = attr
	c.ExtraFiles = files
	tail := new(Tail)
	c.Stdout = tail
	c.Stderr = tail
	c.WaitDelay = time.Second
	if err := childproc.Start(c); err != nil {
		return nil, err
	}
	p := &commandProcess{cmd: c, done: make(chan struct{}), tail: tail, cleanup: cleanup}
	go func() {
		err := childproc.Wait(c)
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}
func (p *commandProcess) Done() <-chan struct{} { return p.done }
func (p *commandProcess) ExitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return fmt.Errorf("%w; helper log: %.1024s", p.err, p.tail.Text())
	}
	return nil
}
func (p *commandProcess) Stop(ctx context.Context) error {
	p.stopMu.Lock()
	defer p.stopMu.Unlock()
	// Never signal a PID after its exit has been reaped (it could be reused).
	if !exited(p) {
		if e := p.cmd.Process.Signal(syscall.SIGTERM); e != nil && !errors.Is(e, os.ErrProcessDone) && !errors.Is(e, syscall.ESRCH) {
			return e
		}
		timer := time.NewTimer(400 * time.Millisecond)
		select {
		case <-p.done:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			if !exited(p) {
				if e := p.cmd.Process.Kill(); e != nil && !errors.Is(e, os.ErrProcessDone) && !errors.Is(e, syscall.ESRCH) {
					return e
				}
			}
			select {
			case <-p.done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if !p.cleaned && p.cleanup != nil {
		if err := p.cleanup(); err != nil {
			return err
		}
	}
	p.cleaned = true
	return nil
}

// ReapOrphans reaps only unowned zombies actually parented to this process.
// It does not steal the status of the supervised hardware helper.
func ReapOrphans(proc string) error { return childproc.ReapOrphans(proc) }

var _ io.Writer = (*Tail)(nil)
