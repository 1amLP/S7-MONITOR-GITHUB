//go:build linux && (amd64 || arm64)

package mediaruntime

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"perimode/native/internal/selinux"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func isMFCName(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Contains(s, "mfc") && !strings.Contains(s, "fimc") && !strings.Contains(s, "secure") && !strings.Contains(s, "drm")
}

// BrokerMain executes INSIDE the private native-media root. Worker children
// therefore share its PID, property and mount namespaces, including Binder PIDs.
// A socket connection alone means only that the broker is alive. AMediaCodec's
// real configure/start result remains the only codec admission result.
func BrokerMain() (ret error) {
	if os.Geteuid() != 0 || os.Getpid() == 1 {
		return errors.New("media broker must be a private-init child")
	}
	if e := selinux.RequireContext(selinux.ClientContext); e != nil {
		return e
	}
	// Original init redirects service stderr to /dev/null. Keep terminal broker
	// errors in the bounded kernel snapshot captured by the outer native PID1.
	var diagnostic io.Writer = os.Stderr
	if f, e := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0); e == nil {
		defer f.Close()
		diagnostic = f
	}
	defer func() {
		if ret != nil {
			fmt.Fprintln(diagnostic, "s7 media broker:", ret)
		}
	}()
	if _, e := os.Stat("/s7/runtime-manifest.json"); e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	listener, e := net.ListenUnix("unix", &net.UnixAddr{Name: "/s7-control/codec.sock", Net: "unix"})
	if e != nil {
		return e
	}
	listener.SetUnlinkOnClose(true)
	defer listener.Close()
	if e = os.Chmod("/s7-control/codec.sock", 0600); e != nil {
		return e
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	brokerReady.Store(false)
	go guardNativeServices(ctx, cancel, diagnostic)
	var roles codecRoles
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	limit := make(chan struct{}, 4)
	for {
		conn, e := listener.AcceptUnix()
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return e
		}
		select {
		case limit <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		wg.Add(1)
		go func(c *net.UnixConn) {
			defer wg.Done()
			defer func() { <-limit }()
			defer c.Close()
			stop := context.AfterFunc(ctx, func() { _ = c.Close() })
			defer stop()
			raw, e := c.SyscallConn()
			if e != nil {
				return
			}
			var cred *syscall.Ucred
			if e = raw.Control(func(fd uintptr) { cred, _ = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED) }); e != nil || cred == nil || cred.Uid != 0 {
				return
			}
			_ = c.SetDeadline(time.Now().Add(time.Second))
			var hello [8]byte
			if _, e = io.ReadFull(c, hello[:]); e != nil || string(hello[:4]) != "S7R3" || hello[4] > 2 || hello[5] != 0 || hello[6] != 0 || hello[7] != 0 {
				return
			}
			role := hello[4]
			if !brokerReady.Load() {
				return
			}
			lease := roles.acquire(role)
			if lease == nil {
				return
			}
			defer lease.abandon()
			if role == 0 {
				if e = writeAll(c, []byte{'S', '7', 'R', 'A', role, 0, 0, 0}); e != nil {
					return
				}
				_ = c.SetDeadline(time.Time{})
				_, _ = io.Copy(io.Discard, c)
				cancel()
				return
			}
			serveCodecFinal(ctx, c, role, func(r byte) *exec.Cmd {
				cmd := exec.Command("/system/bin/linker64", "/s7/mediacodec-bridge", fmt.Sprint(r))
				cmd.Env = []string{"PATH=/nonexistent", "ANDROID_ROOT=/system", "ANDROID_DATA=/data", "ANDROID_RUNTIME_ROOT=/apex/com.android.runtime", "ANDROID_I18N_ROOT=/apex/com.android.i18n"}
				cmd.Stderr = diagnostic
				return cmd
			}, lease.finish)
		}(conn)
	}
}

// ConfirmExit belongs to the session close protocol, not a background monitor.
func ConfirmExit(c net.Conn) error {
	_ = c.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
	var trailer [8]byte
	if _, e := io.ReadFull(c, trailer[:]); e != nil {
		return e
	}
	if string(trailer[:4]) != "S7EX" || binary.LittleEndian.Uint32(trailer[4:]) != 0 {
		return fmt.Errorf("MediaCodec worker cleanup/exit unconfirmed")
	}
	return nil
}

// Original rc services are oneshot: a dead media owner must not restart under
// old codec clients. Watch the exact private-namespace processes, not host PIDs.
func guardNativeServices(ctx context.Context, stop context.CancelFunc, diagnostic io.Writer) {
	owners := map[string]string{}
	deadline := time.Now().Add(10 * time.Second)
	startup, cancelStartup := context.WithDeadline(ctx, deadline)
	defer cancelStartup()
	var registrations registrationGate
	var registrationError error
	lastDiagnostic := ""
	diagnosticCount := 0
	defer brokerReady.Store(false)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		all := true
		entries, e := os.ReadDir("/proc")
		if e != nil {
			fmt.Fprintln(diagnostic, "s7 media process scan:", e)
			stop()
			return
		}
		present := map[string]string{}
		var scanErrors []string
		for _, d := range entries {
			if _, e := strconv.Atoi(d.Name()); e != nil {
				continue
			}
			p, identity, e := readServiceIdentity(d.Name())
			if e == nil {
				if _, duplicate := present[p]; duplicate {
					fmt.Fprintln(diagnostic, "s7 media duplicate service:", p)
					stop()
					return
				}
				present[p] = identity
			} else if len(scanErrors) < 16 {
				scanErrors = append(scanErrors, fmt.Sprintf("pid=%s: %v", d.Name(), e))
			}
		}
		var missing []string
		for _, service := range requiredNativeServices {
			name := service.path
			pid, found := present[name]
			if previous := owners[name]; previous != "" && (!found || previous != pid) {
				fmt.Fprintln(diagnostic, "s7 media service lost/replaced:", name, previous, pid, "scan:", scanErrors)
				stop()
				return
			}
			if !found {
				all = false
				missing = append(missing, name)
			} else {
				owners[name] = pid
			}
		}
		if all && !brokerReady.Load() {
			registrationError = registrations.check(startup, binderRegistered, func(parent context.Context, name string) error {
				step, cancel := context.WithTimeout(parent, 500*time.Millisecond)
				defer cancel()
				return waitHIDL(step, "/system/bin/lshal", name)
			})
			if registrationError == nil {
				registrationError = verifyInitHardening(os.ReadFile)
			}
			if registrationError == nil {
				brokerReady.Store(true)
				fmt.Fprintln(diagnostic, "s7 media registered and hardening verified")
			} else if diagnosticCount < 8 && registrationError.Error() != lastDiagnostic {
				lastDiagnostic = registrationError.Error()
				diagnosticCount++
				fmt.Fprintln(diagnostic, "s7 media pending:", registrations.pending(), "error:", registrationError)
			}
		}
		if (!all || !brokerReady.Load()) && time.Now().After(deadline) {
			fmt.Fprintln(diagnostic, "s7 media startup missing:", missing, "scan:", scanErrors)
			if registrationError != nil {
				fmt.Fprintln(diagnostic, "s7 media registration:", registrationError)
			}
			stop()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
