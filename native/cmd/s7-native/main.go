//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"perimode/native/internal/appliance"
	"perimode/native/internal/mediaruntime"
	"perimode/native/internal/safety"
	"perimode/native/internal/selinux"
	"perimode/native/internal/sensorhub"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == sensorhub.ChildArgument {
		if e := sensorhub.ChildMain(); e != nil {
			log.Print(e)
			os.Exit(2)
		}
		return
	}

	if len(os.Args) == 2 && os.Args[1] == "--media-runtime-child" {
		if e := mediaruntime.ChildMain(); e != nil {
			log.Print(e)
			os.Exit(2)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--media-broker" {
		if e := mediaruntime.BrokerMain(); e != nil {
			log.Print(e)
			os.Exit(2)
		}
		return
	}

	if os.Getenv("SUBSYSTEM") != "" {
		env := map[string]string{}
		for _, v := range os.Environ() {
			k, s, ok := strings.Cut(v, "=")
			if ok {
				env[k] = s
			}
		}
		if e := appliance.FirmwareFallback(env); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--probe" {
		b, e := json.MarshalIndent(appliance.ProbeReport(), "", "  ")
		if e != nil {
			log.Fatal(e)
		}
		fmt.Println(string(b))
		return
	}
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "S7 native /init: run only as PID1; --probe performs host read-only inventory")
		os.Exit(2)
	}
	defer func() {
		if failure := recover(); failure != nil {
			log.Printf("native PID1 panic: %v; requesting Recovery", failure)
			_ = appliance.ReturnToRecovery()
			for {
				time.Sleep(time.Minute)
			}
		}
	}()
	lab, labErr := appliance.LoadLabPolicy()
	if labErr != nil {
		fmt.Fprintln(os.Stderr, "invalid native laboratory policy:", labErr)
		_ = appliance.ReturnToRecovery()
		for {
			time.Sleep(time.Minute)
		}
	}
	halt := appliance.StopMachine
	if lab != nil {
		halt = appliance.ReturnToRecovery
	}
	recordLab := func(stage string, cause error) {
		if lab == nil {
			return
		}
		if err := appliance.RecordLabStatus(*lab, stage, cause); err != nil {
			log.Printf("native laboratory status %s: %v", stage, err)
		}
	}
	recordLabBeforePolicy := func(stage string, cause error) {
		if lab == nil {
			return
		}
		if err := appliance.RecordLabStatusBeforePolicy(*lab, stage, cause); err != nil {
			log.Printf("native laboratory pre-policy status %s: %v", stage, err)
		}
	}
	firstStage := len(os.Args) == 1
	var bootstrapErr error
	if len(os.Args) == 2 && os.Args[1] == selinux.ReadyArg {
		bootstrapErr = selinux.Resume()
		if bootstrapErr == nil {
			recordLab("selinux-ready", nil)
		}
	} else if len(os.Args) != 1 {
		bootstrapErr = errors.New("unrecognized native boot stage")
	} else if bootstrapErr = appliance.SetupRoot(); bootstrapErr == nil {
		if _, bootstrapErr = appliance.Identity(); bootstrapErr == nil {
			recordLabBeforePolicy("bootstrap-root-ready", nil)
			bootstrapErr = selinux.FirstBoot() // Success execs /init; it does not return.
		}
	}
	if e := bootstrapErr; e != nil {
		log.Printf("native root setup: %v", e)
		if firstStage {
			recordLabBeforePolicy("bootstrap-error", e)
		} else {
			recordLab("bootstrap-error", e)
		}
		_ = appliance.ReturnToRecovery()
		for {
			time.Sleep(time.Minute)
		}
	}
	f, e := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0)
	if e == nil {
		log.SetOutput(f)
		// Runtime panics write directly to fd2, bypassing the log package.
		if int(f.Fd()) != 2 {
			if err := syscall.Dup3(int(f.Fd()), 2, 0); err != nil {
				log.Printf("native crash stderr: %v", err)
			}
		}
	}
	if pmsg, err := os.OpenFile("/dev/pmsg0", os.O_WRONLY, 0); err == nil {
		if f != nil {
			log.SetOutput(io.MultiWriter(f, pmsg))
		} else {
			log.SetOutput(pmsg)
		}
		if err := debug.SetCrashOutput(pmsg, debug.CrashOptions{}); err != nil {
			log.Printf("native crash ring: %v", err)
		}
		log.Print("S7 native boot: persistent RAM crash output active")
	} else {
		log.Printf("native crash ring unavailable: %v", err)
	}
	log.SetOutput(io.MultiWriter(log.Writer(), appliance.RuntimeLogWriter()))
	if lab != nil {
		first, err := appliance.BeginLabAttempt(*lab)
		if err != nil || !first {
			statusErr := err
			if statusErr == nil {
				statusErr = errors.New("native laboratory marker already exists")
			}
			recordLab("attempt-refused", statusErr)
			log.Printf("native laboratory attempt refused/repeated: first=%v error=%v", first, err)
			_ = halt()
			for {
				time.Sleep(time.Minute)
			}
		}
		recordLab("attempt-armed", nil)
		if lab.Probe != "" {
			log.Printf("native laboratory %s armed without a session timer", lab.Probe)
		} else {
			log.Printf("native laboratory attempt armed for %d seconds", lab.TrialSeconds)
		}
	}
	sigctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	ctx, cancel := context.WithCancel(sigctx)
	if lab != nil && lab.TrialSeconds > 0 {
		ctx, cancel = context.WithTimeout(sigctx, time.Duration(lab.TrialSeconds)*time.Second)
	}
	defer cancel()
	health := safety.NewLiveness()
	stopWatchdog, watchdogErr := appliance.StartWatchdog(ctx, health, cancel)
	if watchdogErr != nil {
		log.Printf("native startup refused: %v", watchdogErr)
		recordLab("watchdog-error", watchdogErr)
		cancel()
		_ = appliance.ReturnToRecovery()
		for {
			time.Sleep(time.Minute)
		}
	}
	recordLab("watchdog-ready", nil)
	// A fault must not use Samsung's charger poweroff -> native reboot path.
	// Only a confirmed user poweroff selects poweroff as the bounded fallback.
	var faultExit atomic.Bool
	faultExit.Store(true)
	// Bounded final shutdown even if a worker remains inside an old kernel ioctl.
	go func() {
		<-ctx.Done()
		time.Sleep(5 * time.Second)
		log.Print("native shutdown deadline; forcing selected safe exit")
		if faultExit.Load() {
			_ = appliance.ReturnToRecovery()
		} else {
			_ = halt()
		}
	}()
	// Reap unowned firmware-helper zombies without stealing lhd's exit status.
	go func() {
		for ctx.Err() == nil {
			if err := sensorhub.ReapOrphans("/proc"); err != nil {
				log.Printf("orphan reaper: %v", err)
			}
			time.Sleep(time.Second)
		}
	}()

	log.Print("S7 NATIVE LINUX; no Java/ART/APK; optional vendor lhd; automatic settings on serial-pinned CACHE")
	recordLab("run-native-start", nil)
	e = appliance.RunNative(ctx, health)
	faultExit.Store(e != nil)
	recordLab("run-native-stop", e)
	log.Printf("native stopped: %v", e)
	reboot := lab == nil && errors.Is(e, appliance.ErrRebootRequested)
	recovery := recoveryOnResult(e, lab != nil)
	cancel()
	stopWatchdog()
	if recovery {
		if e = appliance.ReturnToRecovery(); e != nil {
			log.Printf("recovery restart failed: %v; requesting poweroff", e)
		}
	}
	if reboot {
		if e = appliance.RestartMachine(); e != nil {
			log.Printf("reboot failed: %v; requesting poweroff", e)
		}
	}
	if e = halt(); e != nil {
		log.Printf("final machine transition failed: %v", e)
	}
	for {
		time.Sleep(time.Minute)
	}
}

func recoveryOnResult(err error, laboratory bool) bool {
	return laboratory || err != nil && !errors.Is(err, appliance.ErrRebootRequested)
}
