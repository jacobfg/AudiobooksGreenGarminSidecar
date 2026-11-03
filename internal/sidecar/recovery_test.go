package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fixture(t *testing.T, f *fakeABS, ext string) {
	t.Helper()
	data, err := os.ReadFile("../../testdata/tone" + ext)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audio = data
	f.extension = ext
}

func startWorker(t *testing.T, s *Server) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Cache.Worker(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker failed to stop")
		}
	})
	return cancel, done
}

func assertUnavailable(t *testing.T, s *Server) {
	t.Helper()
	if m := s.Cache.Manifest("book-a"); m != nil {
		t.Fatalf("partial output published: %+v", m)
	}
	if w := request(s, "/api/items/book-a/file/chunk-001/download", "Bearer valid"); w.Code != 404 {
		t.Fatalf("partial output downloadable: %d", w.Code)
	}
}

func TestUncachedBookQueuesUntilReady(t *testing.T) {
	s, f := setup(t)
	fixture(t, f, ".mp3")
	// Start with a published playlist and simulate a cache entry lost between polls.
	manifest(t, s, "book-a")
	manifest(t, s, "book-b")
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.Cache.root, "book-a")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got := decode(t, request(s, "/audiobookshelf/book?book_id=book-a&skip=0&limit=2", "Bearer valid"))
		if number(got["total"]) != 0 || len(arr(got["files"])) != 0 || number(got["limit"]) != 2 {
			t.Fatal(got)
		}
	}
	if s.Cache.Status("book-a")["state"] != "queued" || len(s.Cache.queue) != 1 {
		t.Fatal("request must queue the book once")
	}
	assertUnavailable(t, s)
	startWorker(t, s)
	waitState(t, s, "book-a", "ready")
	got := decode(t, request(s, "/audiobookshelf/book?book_id=book-a", "Bearer valid"))
	if number(got["total"]) != 1 || obj(arr(got["files"])[0])["id"] != "chunk-001" {
		t.Fatal(got)
	}
	if request(s, "/api/items/book-a/file/chunk-001/download", "Bearer valid").Code != 200 {
		t.Fatal("ready audio unavailable")
	}
}

func TestBookPaginationSelectsFiles(t *testing.T) {
	s, _ := setup(t)
	manifest(t, s, "book-a")
	manifest(t, s, "book-b")
	m := Manifest{}
	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("%04d.mp3", i)
		write(t, filepath.Join(s.Cache.root, "book-a", name), []byte("mp3"))
		m.Files = append(m.Files, File{fmt.Sprintf("chunk-%03d", i), name, name, float64(i)})
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(s.Cache.root, "book-a", "manifest.json"), data)
	if err = s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		skip, limit int
		ids         []string
	}{
		{0, 2, []string{"chunk-001", "chunk-002"}},
		{1, 2, []string{"chunk-002", "chunk-003"}},
		{3, 2, []string{"chunk-004"}},
		{4, 2, []string{}},
		{-1, 1, []string{"chunk-001"}},
		{2, 0, []string{"chunk-003", "chunk-004"}},
	} {
		t.Run(fmt.Sprintf("skip=%d/limit=%d", tc.skip, tc.limit), func(t *testing.T) {
			got := decode(t, request(s, fmt.Sprintf("/audiobookshelf/book?book_id=book-a&skip=%d&limit=%d", tc.skip, tc.limit), "Bearer valid"))
			ids := []string{}
			for _, entry := range arr(got["files"]) {
				f := obj(entry)
				ids = append(ids, str(f["id"]))
				if _, ok := f["path"]; ok {
					t.Fatal("internal cache path exposed")
				}
			}
			if !reflect.DeepEqual(ids, tc.ids) || number(got["total"]) != 4 || number(got["skip"]) != float64(tc.skip) || number(got["limit"]) != float64(tc.limit) {
				t.Fatal(got)
			}
		})
	}
}

func TestDownloadFailureRetries(t *testing.T) {
	for _, kind := range []string{"http-error", "interrupted"} {
		t.Run(kind, func(t *testing.T) {
			s, f := setup(t)
			fixture(t, f, ".mp3")
			if kind == "http-error" {
				f.statusByPath = map[string]int{"/api/items/book-a/file/file-book-a/download": 503}
			} else {
				f.truncateDownload = true
			}
			startWorker(t, s)
			s.Cache.Queue("book-a")
			waitState(t, s, "book-a", "failed")
			assertUnavailable(t, s)
			state := s.Cache.Status("book-a")
			want := "503"
			if kind == "interrupted" {
				want = "unexpected EOF"
			}
			if !strings.Contains(str(state["last_error"]), want) {
				t.Fatal(state)
			}
			if kind == "interrupted" {
				paths, err := filepath.Glob(filepath.Join(s.Cache.root, ".staging", "*", "sources", "*"))
				if err != nil || len(paths) != 1 {
					t.Fatal(paths, err)
				}
				info, err := os.Stat(paths[0])
				if err != nil || info.Size() == 0 {
					t.Fatal("expected partial source for diagnosis", err)
				}
			}
			f.mu.Lock()
			f.statusByPath = nil
			f.truncateDownload = false
			f.mu.Unlock()
			s.Cache.Queue("book-a")
			waitState(t, s, "book-a", "ready")
			if s.Cache.Status("book-a")["last_error"] != nil {
				t.Fatal("stale error after retry")
			}
		})
	}
}

// Wrappers inject failures at a particular encoder invocation while successful
// invocations still use the installed FFmpeg and the actual audio fixtures.
func ffmpegWrapper(t *testing.T, body string) string {
	t.Helper()
	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\nreal='" + strings.ReplaceAll(real, "'", "'\"'\"'") + "'\n" + body + "\n"
	if err = os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestFFmpegFailureDoesNotPublishPartialChunks(t *testing.T) {
	s, f := setup(t)
	fixture(t, f, ".m4b")
	flag := filepath.Join(t.TempDir(), "fail")
	write(t, flag, []byte("fail second chunk"))
	t.Setenv("SIDECAR_TEST_FAIL", flag)
	ffmpegWrapper(t, `for target do :; done
case "$target" in
 */0002.mp3) if [ -f "$SIDECAR_TEST_FAIL" ]; then echo 'injected encoder failure' >&2; exit 1; fi ;;
esac
exec "$real" "$@"`)
	startWorker(t, s)
	s.Cache.Queue("book-a")
	waitState(t, s, "book-a", "failed")
	if !strings.Contains(str(s.Cache.Status("book-a")["last_error"]), "injected encoder failure") {
		t.Fatal(s.Cache.Status("book-a"))
	}
	assertUnavailable(t, s)
	paths, err := filepath.Glob(filepath.Join(s.Cache.root, ".staging", "*", "rendered", "0001.mp3"))
	if err != nil || len(paths) != 1 {
		t.Fatal("first chunk should exist only in staging", paths, err)
	}
	if d, err := duration(context.Background(), paths[0]); err != nil || d <= 0 {
		t.Fatal("first chunk was not rendered", err)
	}
	if err = os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	s.Cache.Queue("book-a")
	waitState(t, s, "book-a", "ready")
	if m := s.Cache.Manifest("book-a"); m == nil || len(m.Files) != 2 {
		t.Fatalf("retry did not publish all chunks: %+v", m)
	}
}

func TestInvalidManifestsAreRebuilt(t *testing.T) {
	for _, kind := range []string{"invalid-json", "duplicate-id", "negative-duration", "missing-file", "directory", "traversal"} {
		t.Run(kind, func(t *testing.T) {
			s, f := setup(t)
			fixture(t, f, ".mp3")
			manifest(t, s, "book-a")
			dir := filepath.Join(s.Cache.root, "book-a")
			m := *s.Cache.Manifest("book-a")
			switch kind {
			case "duplicate-id":
				m.Files = append(m.Files, m.Files[0])
			case "negative-duration":
				m.Files[0].Duration = -1
			case "missing-file":
				m.Files[0].Path = "missing.mp3"
			case "directory":
				m.Files[0].Path = "subdir"
				if err := os.Mkdir(filepath.Join(dir, "subdir"), 0755); err != nil {
					t.Fatal(err)
				}
			case "traversal":
				m.Files[0].Path = "../outside.mp3"
			}
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "invalid-json" {
				data = []byte("{broken")
			}
			write(t, filepath.Join(dir, "manifest.json"), data)
			assertUnavailable(t, s)
			startWorker(t, s)
			s.Cache.Queue("book-a")
			waitState(t, s, "book-a", "ready")
			path := s.Cache.File("book-a", "chunk-001")
			if d, err := duration(context.Background(), path); err != nil || d <= 0 {
				t.Fatal("cache not rebuilt with valid audio", err)
			}
		})
	}
}

func TestUnsupportedSourceFailsBeforeDownload(t *testing.T) {
	for _, kind := range []string{"unsupported-extension", "missing-audio", "missing-file-id"} {
		t.Run(kind, func(t *testing.T) {
			s, f := setup(t)
			switch kind {
			case "unsupported-extension":
				f.extension = ".flac"
			case "missing-audio":
				f.audioFiles = []Object{}
			case "missing-file-id":
				f.audioFiles = []Object{{"metadata": Object{"filename": "book.mp3"}}}
			}
			startWorker(t, s)
			s.Cache.Queue("book-a")
			waitState(t, s, "book-a", "failed")
			assertUnavailable(t, s)
			f.mu.Lock()
			downloads := f.downloads
			f.mu.Unlock()
			if downloads != 0 {
				t.Fatalf("invalid source downloaded %d times", downloads)
			}
			want := "only M4B and MP3"
			if kind == "missing-audio" {
				want = "no audio files"
			}
			if !strings.Contains(str(s.Cache.Status("book-a")["last_error"]), want) {
				t.Fatal(s.Cache.Status("book-a"))
			}
		})
	}
}

func TestCancelDuringEncodingStopsFFmpeg(t *testing.T) {
	s, f := setup(t)
	fixture(t, f, ".mp3")
	marker := filepath.Join(t.TempDir(), "encoder.pid")
	t.Setenv("SIDECAR_TEST_PID", marker)
	ffmpegWrapper(t, `echo $$ > "$SIDECAR_TEST_PID"
exec "$real" -re "$@"`)
	cancel, done := startWorker(t, s)
	s.Cache.Queue("book-a")
	// Wait for an actual output file, not merely a queued job or wrapper process.
	deadline := time.Now().Add(5 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		paths, _ := filepath.Glob(filepath.Join(s.Cache.root, ".staging", "*", "rendered", "0001.mp3"))
		if len(paths) == 1 {
			started = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !started {
		t.Fatal("FFmpeg did not begin encoding")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(pid, 0); err != nil {
		t.Fatal("encoder already exited before cancellation", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop worker")
	}
	if err = syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("encoder still exists after shutdown: %v", err)
	}
	assertUnavailable(t, s)
	if state := s.Cache.Status("book-a"); state["state"] != "failed" {
		t.Fatal(state)
	}
	// A fresh worker can retry the interrupted job without reusing partial output.
	startWorker(t, s)
	s.Cache.Queue("book-a")
	waitState(t, s, "book-a", "ready")
}

func TestStatusUpstreamFailuresPreserveBooksAndAudio(t *testing.T) {
	for _, path := range []string{"/api/items/book-a", "/api/me/progress/book-a"} {
		t.Run(path, func(t *testing.T) {
			s, f := setup(t)
			manifest(t, s, "book-a")
			manifest(t, s, "book-b")
			if err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			before, _ := s.snapshot("garmin-adventure")
			f.mu.Lock()
			f.statusByPath = map[string]int{path: 503}
			f.mu.Unlock()
			w := request(s, "/garmin/playlist-status/Adventure", "Bearer valid")
			if w.Code != 502 || !strings.Contains(w.Body.String(), "Audiobookshelf request failed") {
				t.Fatalf("unexpected failure: %d %s", w.Code, w.Body.String())
			}
			after, _ := s.snapshot("garmin-adventure")
			if !reflect.DeepEqual(before, after) {
				t.Fatal("playlist changed after upstream failure")
			}
			for _, id := range []string{"book-a", "book-b"} {
				w := request(s, "/api/items/"+id+"/file/chunk-001/download", "Bearer valid")
				if w.Code != 200 || w.Body.String() != "mp3" {
					t.Fatal("cached audio lost")
				}
			}
			f.mu.Lock()
			f.statusByPath = nil
			f.mu.Unlock()
			got := decode(t, request(s, "/garmin/playlist-status/Adventure", "Bearer valid"))
			if number(got["book_count"]) != 2 {
				t.Fatal(got)
			}
		})
	}
}
