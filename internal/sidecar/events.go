package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// ABS uses Engine.IO v4 and Socket.IO v5 JSON events in the default namespace.
// Connect directly over WebSocket; REST polling remains the fallback. Binary
// events and other namespaces are intentionally outside this client's scope.
func (a *ABS) events(ctx context.Context, changed func(string), state func(string)) {
	delay := time.Second
	defer state("disconnected")
	for ctx.Err() == nil {
		state("connecting")
		connectedAt := time.Now()
		authenticated := false
		_ = a.eventSession(ctx, changed, func() { authenticated = true; state("connected") })
		if ctx.Err() != nil {
			return
		}
		state("disconnected")
		if authenticated && time.Since(connectedAt) > time.Minute {
			delay = time.Second
		}
		timer := time.NewTimer(delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(delay*2, time.Minute)
	}
}
func socketURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("ABS_URL must use HTTP or HTTPS")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/socket.io/"
	u.RawPath = ""
	u.RawQuery = "EIO=4&transport=websocket"
	u.Fragment = ""
	return u.String(), nil
}
func (a *ABS) eventSession(ctx context.Context, changed func(string), ready func()) error {
	address, err := socketURL(a.URL)
	if err != nil {
		return err
	}
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second
	conn, response, err := dialer.DialContext(ctx, address, nil)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	conn.SetReadLimit(16 << 20)
	send := func(packet string) error {
		if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		return conn.WriteMessage(websocket.TextMessage, []byte(packet))
	}
	authDeadline := time.Now().Add(10 * time.Second)
	_ = conn.SetReadDeadline(authDeadline)
	kind, data, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	if kind != websocket.TextMessage || len(data) < 2 || data[0] != '0' {
		return fmt.Errorf("invalid Engine.IO handshake")
	}
	var handshake struct {
		SID      string `json:"sid"`
		Interval int64  `json:"pingInterval"`
		Timeout  int64  `json:"pingTimeout"`
	}
	if json.Unmarshal(data[1:], &handshake) != nil || handshake.SID == "" || handshake.Interval < 1 || handshake.Timeout < 1 || handshake.Interval > 300000 || handshake.Timeout > 300000 {
		return fmt.Errorf("invalid Engine.IO heartbeat settings")
	}
	heartbeat := time.Duration(handshake.Interval+handshake.Timeout) * time.Millisecond
	pingDeadline := time.Now().Add(heartbeat)
	if err = send("40"); err != nil {
		return err
	}
	authed, sentAuth := false, false
	for {
		deadline := pingDeadline
		if !authed && authDeadline.Before(deadline) {
			deadline = authDeadline
		}
		_ = conn.SetReadDeadline(deadline)
		kind, data, err = conn.ReadMessage()
		if err != nil {
			return err
		}
		if kind != websocket.TextMessage || len(data) == 0 {
			return fmt.Errorf("unsupported socket packet")
		}
		packet := string(data)
		switch {
		case packet == "2":
			if err = send("3"); err != nil {
				return err
			}
			pingDeadline = time.Now().Add(heartbeat)
		case packet == "6": // Engine.IO noop
		case packet == "1" || strings.HasPrefix(packet, "41") || strings.HasPrefix(packet, "44"):
			return fmt.Errorf("ABS socket disconnected or rejected connection")
		case strings.HasPrefix(packet, "40"):
			if sentAuth {
				return fmt.Errorf("duplicate Socket.IO connection")
			}
			payload, _ := json.Marshal([]string{"auth", strings.TrimSpace(strings.TrimPrefix(a.Token, "Bearer "))})
			if err = send("42" + string(payload)); err != nil {
				return err
			}
			sentAuth = true
		case strings.HasPrefix(packet, "42"):
			payload := packet[2:]
			// Answer optional acknowledgements after processing the JSON event.
			n := 0
			for n < len(payload) && payload[n] >= '0' && payload[n] <= '9' {
				n++
			}
			ack := payload[:n]
			payload = payload[n:]
			var event []json.RawMessage
			if json.Unmarshal([]byte(payload), &event) != nil || len(event) == 0 {
				return fmt.Errorf("invalid Socket.IO event")
			}
			var name string
			if json.Unmarshal(event[0], &name) != nil {
				return fmt.Errorf("invalid event name")
			}
			if name == "auth_failed" {
				return fmt.Errorf("ABS socket authentication failed")
			}
			if name == "init" && sentAuth && !authed {
				authed = true
				ready()
				changed(name)
			} else if authed && refreshEvent(name) {
				changed(name)
			}
			if ack != "" {
				if err = send("43" + ack + "[]"); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unsupported Socket.IO packet")
		}
	}
}
func refreshEvent(name string) bool {
	switch name {
	case "playlist_added", "playlist_updated", "playlist_removed", "item_added", "item_updated", "item_removed", "items_added", "items_updated", "items_removed", "library_added", "library_updated", "library_removed", "scan_complete":
		return true
	default:
		return false
	}
}
