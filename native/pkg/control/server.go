package control

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"perimode/native/pkg/monitor"
	"io"
	"net"
	"time"
)

const Address = "127.0.0.1:39572"

type Request struct {
	Command string `json:"command"`
	Profile string `json:"profile,omitempty"`
}
type Response struct {
	Video            *monitor.Status `json:"video,omitempty"`
	MonitorWanted    bool            `json:"monitor_wanted"`
	HomeReady        bool            `json:"home_ready"`
	HomeError        string          `json:"home_error,omitempty"`
	OK               bool            `json:"ok"`
	Power            bool            `json:"power"`
	Audio            bool            `json:"audio"`
	Touch            bool            `json:"touch"`
	Touchpad         bool            `json:"touchpad"`
	TouchActive      bool            `json:"touch_active"`
	MonitorControl   bool            `json:"monitor_control"`
	MonitorConnected bool            `json:"monitor_connected"`
	MonitorState     string          `json:"monitor_state,omitempty"`
	MonitorError     string          `json:"monitor_error,omitempty"`
	UVC              bool            `json:"uvc"`
	UVCError         string          `json:"uvc_error,omitempty"`
	CPU              *CPUReading     `json:"cpu,omitempty"`
	Profile          string          `json:"profile,omitempty"`
	ProfileControl   bool            `json:"profile_control"`
	ProfileError     string          `json:"profile_error,omitempty"`
}

func ValidProfile(profile string) bool {
	switch profile {
	case "h264-720p30", "h264-720p60", "h264-1080p30", "h264-1080p60", "h264-1440p30":
		return true
	}
	return false
}

func handle(conn net.Conn, token []byte, power func(string) error, status func() Response, profile func(string) error, monitor ...func(string) error) error {
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	var supplied [32]byte
	if _, err := io.ReadFull(conn, supplied[:]); err != nil {
		return err
	}
	if len(token) != 32 || subtle.ConstantTimeCompare(token, supplied[:]) != 1 {
		return errors.New("control authentication failed")
	}
	line, err := bufio.NewReaderSize(conn, 2048).ReadSlice('\n')
	if err != nil {
		return err
	}
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("extra control data")
	}
	if request.Command != "camera_profile" && request.Profile != "" {
		return errors.New("profile supplied to another command")
	}
	switch request.Command {
	case "monitor_connect", "monitor_disconnect", "monitor_reconnect", "usb_toggle":
		if len(monitor) != 1 || monitor[0] == nil {
			return errors.New("monitor control unavailable")
		}
		if err := monitor[0](request.Command); err != nil {
			return err
		}
		return json.NewEncoder(conn).Encode(Response{OK: true, MonitorControl: true})
	case "status":
		response := Response{}
		if status != nil {
			response = status()
		}
		response.OK, response.Power = true, power != nil
		return json.NewEncoder(conn).Encode(response)
	case "phone_reboot", "phone_poweroff":
		if power == nil {
			return errors.New("power backend unavailable")
		}
		if err := json.NewEncoder(conn).Encode(Response{OK: true, Power: true}); err != nil {
			return err
		}
		return power(request.Command)
	case "camera_profile":
		if profile == nil || !ValidProfile(request.Profile) {
			return errors.New("profile not supported")
		}
		// As with power, acknowledge the accepted command before USB can disappear.
		// The UI confirms application from the later status, not this acceptance.
		if err := json.NewEncoder(conn).Encode(Response{OK: true}); err != nil {
			return err
		}
		return profile(request.Profile)
	default:
		return errors.New("unknown control command")
	}
}

func Serve(ctx context.Context, token []byte, power func(string) error, status func() Response, profile func(string) error, monitor ...func(string) error) error {
	listener, err := net.Listen("tcp4", Address)
	if err != nil {
		return err
	}
	return ServeListener(ctx, listener, token, power, status, profile, monitor...)
}

func ServeListener(ctx context.Context, listener net.Listener, token []byte, power func(string) error, status func() Response, profile func(string) error, monitor ...func(string) error) error {
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	// Bound concurrency; no single stalled status client owns the controller.
	slots := make(chan struct{}, 8)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			stopConn := context.AfterFunc(ctx, func() { conn.Close() })
			defer stopConn()
			_ = handle(conn, token, power, status, profile, monitor...)
		}()
	}
}
