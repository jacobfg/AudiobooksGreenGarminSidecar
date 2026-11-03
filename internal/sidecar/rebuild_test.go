package sidecar

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
)

func audioVersion(version int) []Object {
	return []Object{{"ino": "file-book-a", "duration": 3661.8, "metadata": Object{"filename": "book-a.mp3", "mtimeMs": version, "size": 16552}}}
}
func TestAudioFingerprintIgnoresMetadata(t *testing.T) {
	s, _ := setup(t)
	data, _ := json.Marshal(bookData("book-a", ".mp3"))
	var item Object
	_ = json.Unmarshal(data, &item)
	before := sourceFingerprint(item, s.Settings)
	item["updatedAt"] = 999.
	item["progress"] = 0.5
	media := obj(item["media"])
	media["metadata"] = Object{"title": "Renamed", "authors": []any{"new author"}}
	media["coverPath"] = "new-cover.jpg"
	if sourceFingerprint(item, s.Settings) != before {
		t.Fatal("metadata-only update invalidates audio")
	}
	obj(obj(arr(media["audioFiles"])[0])["metadata"])["mtimeMs"] = 999.
	if sourceFingerprint(item, s.Settings) == before {
		t.Fatal("audio replacement did not invalidate")
	}
}
func TestRebuildPreservesReadyAudioAndOpenDownloads(t *testing.T) {
	s, f := setup(t)
	fixture(t, f, ".mp3")
	f.items = map[string][]string{"first": {"book-a"}}
	f.audioFiles = audioVersion(1)
	flag := filepath.Join(t.TempDir(), "fail")
	t.Setenv("SIDECAR_TEST_FAIL", flag)
	ffmpegWrapper(t, `if [ -f "$SIDECAR_TEST_FAIL" ]; then echo 'injected failure' >&2; exit 1; fi
exec "$real" "$@"`)
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	startWorker(t, s)
	waitState(t, s, "book-a", "ready")
	before := s.Cache.Manifest("book-a")
	oldPath := s.Cache.File("book-a", "chunk-001")
	oldBytes, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	handle, _, err := s.Cache.Open("book-a", "chunk-001")
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err = s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	downloads := f.downloads
	f.mu.Unlock()
	if downloads != 1 {
		t.Fatal(downloads)
	}
	write(t, flag, []byte("fail"))
	f.mu.Lock()
	f.audioFiles = audioVersion(2)
	f.mu.Unlock()
	if err = s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return s.Cache.Status("book-a")["refresh_state"] == "failed" })
	if m := s.Cache.Manifest("book-a"); m == nil || m.Fingerprint != before.Fingerprint {
		t.Fatal("failed rebuild replaced good cache")
	}
	w := request(s, "/api/items/book-a/file/chunk-001/download", "Bearer valid")
	if w.Code != 200 || !reflect.DeepEqual(w.Body.Bytes(), oldBytes) {
		t.Fatal("old rendition unavailable during retry")
	}
	if err = os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	if err = s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { m := s.Cache.Manifest("book-a"); return m != nil && m.Fingerprint != before.Fingerprint })
	actual, err := io.ReadAll(handle)
	if err != nil || !reflect.DeepEqual(actual, oldBytes) {
		t.Fatal("open download disrupted", err)
	}
	if _, err = os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("obsolete generation not cleaned", err)
	}
	if m := s.Cache.Manifest("book-a"); m == nil || len(m.Files) != 1 {
		t.Fatal("new rendition invalid")
	}
}
func TestChangesDuringConversionCancelObsoleteWork(t *testing.T) {
	for _, change := range []string{"delete", "new-source", "readd"} {
		t.Run(change, func(t *testing.T) {
			s, f := setup(t)
			fixture(t, f, ".mp3")
			f.items = map[string][]string{"first": {"book-a"}}
			f.audioFiles = audioVersion(1)
			upstream := s.ABS.URL
			started, cancelled := make(chan struct{}), make(chan struct{})
			var downloads atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/items/book-a/file/file-book-a/download" && downloads.Add(1) == 1 {
					close(started)
					<-r.Context().Done()
					close(cancelled)
					return
				}
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
			}))
			t.Cleanup(proxy.Close)
			s.ABS.URL = proxy.URL
			if err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			startWorker(t, s)
			eventually(t, func() bool {
				select {
				case <-started:
					return true
				default:
					return false
				}
			})
			f.mu.Lock()
			if change == "delete" || change == "readd" {
				f.playlists = []Object{}
			} else {
				f.audioFiles = audioVersion(2)
			}
			f.mu.Unlock()
			if err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if change == "readd" {
				f.mu.Lock()
				f.playlists = []Object{{"id": "first", "libraryId": "a", "name": "Adventure"}}
				f.mu.Unlock()
				if err := s.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			eventually(t, func() bool {
				select {
				case <-cancelled:
					return true
				default:
					return false
				}
			})
			if change == "delete" {
				eventually(t, func() bool { s.Cache.mu.Lock(); defer s.Cache.mu.Unlock(); return !s.Cache.running["book-a"] })
				assertUnavailable(t, s)
				s.Cache.Queue("book-a")
				s.Cache.mu.Lock()
				pending := s.Cache.pending["book-a"]
				s.Cache.mu.Unlock()
				if pending {
					t.Fatal("removed item can be queued")
				}
			} else {
				waitState(t, s, "book-a", "ready")
				item, err := s.ABS.Item(context.Background(), "book-a")
				if err != nil {
					t.Fatal(err)
				}
				if s.Cache.Manifest("book-a").Fingerprint != sourceFingerprint(item, s.Settings) {
					t.Fatal("obsolete version published")
				}
			}
		})
	}
}
