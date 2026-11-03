package sidecar

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func socketFixture(t *testing.T, session func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audiobookshelf/socket.io/" || r.URL.Query().Get("EIO") != "4" || r.URL.Query().Get("transport") != "websocket" {
			t.Errorf("unexpected socket URL: %s", r.URL)
			w.WriteHeader(404)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		session(conn)
	}))
	t.Cleanup(up.Close)
	return up
}
func socketSend(t *testing.T, c *websocket.Conn, packet string) {
	t.Helper()
	if err := c.WriteMessage(websocket.TextMessage, []byte(packet)); err != nil {
		t.Error(err)
	}
}
func socketRead(t *testing.T, c *websocket.Conn, want string) bool {
	t.Helper()
	_, data, err := c.ReadMessage()
	if err != nil || string(data) != want {
		t.Errorf("wanted %q, got %q (%v)", want, data, err)
		return false
	}
	return true
}
func socketHandshake(t *testing.T, c *websocket.Conn, heartbeat int) bool {
	socketSend(t, c, `0{"sid":"test","pingInterval":`+fmtInt(heartbeat)+`,"pingTimeout":`+fmtInt(heartbeat)+`}`)
	if !socketRead(t, c, "40") {
		return false
	}
	socketSend(t, c, `40{"sid":"namespace"}`)
	return socketRead(t, c, `42["auth","test-token"]`)
}
func fmtInt(v int) string { data, _ := json.Marshal(v); return string(data) }
func TestSocketAuthEventsAndHeartbeat(t *testing.T) {
	up := socketFixture(t, func(c *websocket.Conn) {
		if !socketHandshake(t, c, 1000) {
			return
		}
		socketSend(t, c, `42["playlist_updated",{}]`)
		socketSend(t, c, `42["init",{"userId":"test"}]`)
		socketSend(t, c, "2")
		if !socketRead(t, c, "3") {
			return
		}
		socketSend(t, c, `42["user_item_progress_updated",{}]`)
		socketSend(t, c, `427["playlist_updated",{"id":"first"}]`)
		if !socketRead(t, c, "437[]") {
			return
		}
		socketSend(t, c, "41")
	})
	changes, ready := 0, 0
	err := NewABS(up.URL+"/audiobookshelf", "test-token").eventSession(context.Background(), func() { changes++ }, func() { ready++ })
	if err == nil || changes != 2 || ready != 1 {
		t.Fatal(changes, ready, err)
	}
}
func TestSocketFailureModes(t *testing.T) {
	for _, mode := range []string{"authentication", "malformed", "heartbeat"} {
		t.Run(mode, func(t *testing.T) {
			up := socketFixture(t, func(c *websocket.Conn) {
				if !socketHandshake(t, c, 20) {
					return
				}
				if mode == "authentication" {
					socketSend(t, c, `42["auth_failed",{"message":"no"}]`)
					return
				}
				socketSend(t, c, `42["init",{}]`)
				if mode == "malformed" {
					socketSend(t, c, `42[broken`)
					return
				}
				_, _, _ = c.ReadMessage()
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			count := 0
			err := NewABS(up.URL+"/audiobookshelf", "test-token").eventSession(ctx, func() { count++ }, func() {})
			if err == nil || ctx.Err() != nil {
				t.Fatal("session did not reject failed protocol promptly", err)
			}
			if mode == "authentication" && count != 0 {
				t.Fatal("unauthenticated refresh")
			}
		})
	}
}
func TestSocketReconnectReauthenticates(t *testing.T) {
	var connections atomic.Int32
	up := socketFixture(t, func(c *websocket.Conn) {
		n := connections.Add(1)
		if !socketHandshake(t, c, 1000) {
			return
		}
		socketSend(t, c, `42["init",{}]`)
		if n == 1 {
			return
		}
		_, _, _ = c.ReadMessage()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan struct{}, 10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewABS(up.URL+"/audiobookshelf", "test-token").events(ctx, func() { changed <- struct{}{} }, func(string) {})
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-changed:
		case <-time.After(4 * time.Second):
			t.Fatal("reconnect did not refresh")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("listener did not stop")
	}
	if connections.Load() < 2 {
		t.Fatal("did not reconnect")
	}
}
func TestOfficialSocketIO(t *testing.T) {
	if os.Getenv("SIDECAR_SOCKETIO_TEST") != "1" {
		t.Skip("set SIDECAR_SOCKETIO_TEST=1 after npm ci --prefix testdata/socketio")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "../../testdata/socketio/server.cjs")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("Socket.IO server did not start", scanner.Err())
	}
	a := NewABS(scanner.Text(), "reference-token")
	ready, changed := 0, 0
	err = a.eventSession(ctx, func() {
		changed++
		if changed == 2 {
			cancel()
		}
	}, func() { ready++ })
	if changed != 2 || ready != 1 {
		t.Fatalf("official Socket.IO interoperability failed: ready=%d changed=%d err=%v", ready, changed, err)
	}
}
func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func runServer(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
}
func TestEventsDuringRefreshAreNotLost(t *testing.T) {
	s, f := setup(t)
	f.items = map[string][]string{}
	original := s.ABS.URL
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/libraries" && calls.Add(1) == 2 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		req, _ := http.NewRequestWithContext(r.Context(), "GET", original+r.URL.RequestURI(), nil)
		req.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(proxy.Close)
	s.ABS.URL = proxy.URL
	runServer(t, s)
	eventually(t, func() bool { return !s.synchronization().LastSuccess.IsZero() })
	for i := 0; i < 100; i++ {
		s.notify()
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("event did not refresh")
	}
	for i := 0; i < 100; i++ {
		s.notify()
	}
	close(release)
	eventually(t, func() bool { return calls.Load() >= 3 })
	if calls.Load() != 3 {
		t.Fatalf("burst produced excessive refreshes: %d", calls.Load())
	}
}
func TestPollingRecoversMissedEvents(t *testing.T) {
	s, f := setup(t)
	f.items = map[string][]string{}
	f.playlists = []Object{}
	s.Settings.PollInterval = 25 * time.Millisecond
	runServer(t, s)
	eventually(t, func() bool { return !s.synchronization().LastSuccess.IsZero() })
	f.mu.Lock()
	f.items = map[string][]string{"first": {"book-a"}}
	f.playlists = []Object{{"id": "first", "libraryId": "a", "name": "Adventure"}}
	f.mu.Unlock()
	eventually(t, func() bool { books, _ := s.snapshot("garmin-adventure"); return len(books) == 1 })
	f.mu.Lock()
	f.playlists = []Object{}
	f.mu.Unlock()
	eventually(t, func() bool { books, _ := s.snapshot("garmin-adventure"); return len(books) == 0 })
	w := request(s, "/garmin/status", "Bearer valid")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "last_successful_refresh") {
		t.Fatal(w)
	}
	if request(s, "/garmin/status", "").Code != 401 {
		t.Fatal("sync status must require authentication")
	}
}

func TestLiveEventsQueueConversionAndDeleteCache(t *testing.T) {
	s, f := setup(t)
	fixture(t, f, ".mp3")
	f.playlists = []Object{}
	f.items = map[string][]string{"first": {"book-a"}}
	upstream := s.ABS.URL
	notifications := make(chan string, 4)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/socket.io/" {
			req, _ := http.NewRequestWithContext(r.Context(), "GET", upstream+r.URL.RequestURI(), nil)
			req.Header = r.Header.Clone()
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				w.WriteHeader(502)
				return
			}
			defer response.Body.Close()
			w.WriteHeader(response.StatusCode)
			_, _ = io.Copy(w, response.Body)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		socketSend(t, conn, `0{"sid":"integration","pingInterval":25000,"pingTimeout":20000}`)
		if !socketRead(t, conn, "40") {
			return
		}
		socketSend(t, conn, `40{"sid":"integration"}`)
		if !socketRead(t, conn, `42["auth","service-token"]`) {
			return
		}
		socketSend(t, conn, `42["init",{}]`)
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		for {
			select {
			case <-closed:
				return
			case packet := <-notifications:
				socketSend(t, conn, packet)
			}
		}
	}))
	t.Cleanup(proxy.Close)
	s.ABS.URL = proxy.URL
	runServer(t, s)
	eventually(t, func() bool {
		return s.synchronization().Socket == "connected" && !s.synchronization().LastSuccess.IsZero()
	})
	f.mu.Lock()
	f.playlists = []Object{{"id": "first", "libraryId": "a", "name": "Adventure"}}
	f.mu.Unlock()
	notifications <- `42["playlist_updated",{"id":"first"}]`
	waitState(t, s, "book-a", "ready")
	if request(s, "/api/items/book-a/file/chunk-001/download", "Bearer valid").Code != 200 {
		t.Fatal("event-triggered conversion not downloadable")
	}
	f.mu.Lock()
	f.playlists = []Object{}
	f.mu.Unlock()
	notifications <- `42["playlist_removed",{"id":"first"}]`
	eventually(t, func() bool {
		books, _ := s.snapshot("garmin-adventure")
		return len(books) == 0 && s.Cache.Manifest("book-a") == nil
	})
	assertUnavailable(t, s)
}
