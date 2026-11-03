package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeABS struct {
	mu                   sync.Mutex
	libraries, playlists any
	items                map[string][]string
	status               int
	authCalls            int
	audio                []byte
	extension            string
	downloads            int
	statusByPath         map[string]int
	truncateDownload     bool
	audioFiles           any
}

func bookData(id, ext string) Object {
	return Object{"id": id, "media": Object{"metadata": Object{"title": strings.ToUpper(id), "authors": []Object{{"name": "Test Author"}}}, "audioFiles": []Object{{"ino": "file-" + id, "duration": 3661.8, "metadata": Object{"filename": id + ext}}}}}
}
func setup(t *testing.T) (*Server, *fakeABS) {
	t.Helper()
	f := &fakeABS{libraries: []Object{{"id": "a", "name": "Library A"}, {"id": "b", "name": "Library B"}}, playlists: []Object{{"id": "first", "libraryId": "a", "name": "Adventure"}, {"id": "second", "libraryId": "b", "name": "Adventure"}}, items: map[string][]string{"first": {"book-a"}, "second": {"book-a", "book-b"}}, extension: ".m4b"}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/socket.io/") {
			w.WriteHeader(503)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		token := r.Header.Get("Authorization")
		if r.URL.Path == "/api/me" {
			f.authCalls++
			if token != "Bearer valid" {
				w.WriteHeader(401)
				return
			}
			respond(w, 200, Object{"id": "user"})
			return
		}
		if token != "Bearer service-token" {
			t.Errorf("unexpected service auth: %q", token)
			w.WriteHeader(401)
			return
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		if status := f.statusByPath[r.URL.Path]; status != 0 {
			w.WriteHeader(status)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/libraries/") && strings.HasSuffix(r.URL.Path, "/playlists") {
			library := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/libraries/"), "/playlists")
			entries, ok := f.playlists.([]Object)
			if !ok {
				respond(w, 200, Object{"results": nil})
				return
			}
			matched := []Object{}
			for _, p := range entries {
				if str(p["libraryId"]) == library {
					matched = append(matched, p)
				}
			}
			respond(w, 200, Object{"results": matched, "total": len(matched)})
			return
		}
		switch r.URL.Path {
		case "/api/libraries":
			respond(w, 200, Object{"libraries": f.libraries})
		case "/api/playlists":
			respond(w, 200, Object{"playlists": f.playlists})
		default:
			if strings.HasPrefix(r.URL.Path, "/api/playlists/") {
				ids, ok := f.items[strings.TrimPrefix(r.URL.Path, "/api/playlists/")]
				if !ok {
					w.WriteHeader(404)
					return
				}
				items := []Object{}
				for _, id := range ids {
					items = append(items, Object{"libraryItem": bookData(id, f.extension)})
				}
				respond(w, 200, Object{"items": items})
			} else if strings.HasSuffix(r.URL.Path, "/download") {
				f.downloads++
				if f.truncateDownload {
					w.Header().Set("Content-Length", fmt.Sprint(len(f.audio)+100))
					_, _ = w.Write(f.audio[:len(f.audio)/2])
					return
				}
				_, _ = w.Write(f.audio)
			} else if strings.HasPrefix(r.URL.Path, "/api/items/") {
				item := bookData(strings.TrimPrefix(r.URL.Path, "/api/items/"), f.extension)
				if f.audioFiles != nil {
					obj(item["media"])["audioFiles"] = f.audioFiles
				}
				respond(w, 200, item)
			} else if strings.HasPrefix(r.URL.Path, "/api/me/progress/") {
				if strings.HasSuffix(r.URL.Path, "book-a") {
					respond(w, 200, Object{"currentTime": 65.9, "progress": 0.018})
				} else {
					w.WriteHeader(404)
				}
			} else {
				w.WriteHeader(404)
			}
		}
	}))
	t.Cleanup(up.Close)
	s, err := NewServer(Settings{ABSURL: up.URL, Token: "service-token", CacheDir: t.TempDir(), Bitrate: "96k", PollInterval: time.Hour, AuthCache: time.Minute, ChunkMinutes: 30, Playlists: []Playlist{{"garmin-adventure", "Adventure", []string{"first", "second"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}
func request(s *Server, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func decode(t *testing.T, w *httptest.ResponseRecorder) Object {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var v Object
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func manifest(t *testing.T, s *Server, id string) {
	t.Helper()
	dir := filepath.Join(s.Cache.root, id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "0001.mp3"), []byte("mp3"))
	item, err := s.ABS.Item(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(Manifest{Fingerprint: sourceFingerprint(item, s.Settings), Files: []File{{"chunk-001", "0001.mp3", "0001.mp3", 12.5}}})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "manifest.json"), data)
}
func TestAPI(t *testing.T) {
	s, f := setup(t)
	manifest(t, s, "book-a")
	manifest(t, s, "book-b")
	w := request(s, "/audiobookshelf/playlists", "Bearer valid")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"libraryId":"garmin-sidecar"`) {
		t.Fatal(w.Body.String())
	}
	w = request(s, "/audiobookshelf/playlist?playlist_id=garmin-adventure", "Bearer valid")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var books []Object
	_ = json.Unmarshal(w.Body.Bytes(), &books)
	if len(books) != 2 || books[0]["id"] != "book-a" || books[1]["id"] != "book-b" {
		t.Fatal(books)
	}
	for _, query := range []string{"", "&limit=30", "&skip=-1", "&skip=1", "&skip=999999", "&limit=-1"} {
		v := decode(t, request(s, "/audiobookshelf/book?book_id=book-a"+query, "Bearer valid"))
		if number(v["total"]) != 1 {
			t.Fatal(v)
		}
		expected := 1
		if strings.Contains(query, "skip=1") || strings.Contains(query, "skip=999") {
			expected = 0
		}
		if len(arr(v["files"])) != expected {
			t.Fatal(v)
		}
	}
	w = request(s, "/api/items/book-a/file/chunk-001/download", "Bearer valid")
	if w.Code != 200 || w.Body.String() != "mp3" || w.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatal(w)
	}
	f.mu.Lock()
	calls := f.authCalls
	f.mu.Unlock()
	if calls != 1 {
		t.Fatalf("token not cached: %d", calls)
	}
	v := decode(t, request(s, "/garmin/playlist-status/adventure", "Bearer valid"))
	first := obj(arr(v["books"])[0])
	if first["duration"] != "1:01:01" || first["position"] != "0:01:05" || obj(first["cache"])["state"] != "ready" {
		t.Fatal(first)
	}
	if obj(arr(v["books"])[1])["position"] != "0:00:00" {
		t.Fatal(v)
	}
}
func TestAuthAndErrors(t *testing.T) {
	s, _ := setup(t)
	for _, path := range []string{"/audiobookshelf/playlists", "/audiobookshelf/playlist?playlist_id=garmin-adventure", "/audiobookshelf/book?book_id=book-a", "/garmin/playlist-status/Adventure", "/api/items/book-a/file/chunk-001/download"} {
		for _, token := range []string{"", "Bearer no", "Bearer ", "Basic valid"} {
			if w := request(s, path, token); w.Code != 401 {
				t.Fatalf("%s %q: %d", path, token, w.Code)
			}
		}
	}
	if request(s, "/healthz", "").Code != 200 {
		t.Fatal("health")
	}
	for path, status := range map[string]int{"/audiobookshelf/playlist?playlist_id=unknown": 404, "/garmin/playlist-status/unknown": 404, "/api/items/book-a/file/no/download": 404, "/audiobookshelf/playlist": 422, "/audiobookshelf/book": 422, "/audiobookshelf/book?book_id=a&skip=no": 422} {
		if w := request(s, path, "Bearer valid"); w.Code != status {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	for _, id := range []string{"unknown", "book-a"} {
		v := decode(t, request(s, "/audiobookshelf/book?book_id="+id, "Bearer valid"))
		if number(v["total"]) != 0 {
			t.Fatal(v)
		}
	}
}
func TestOnlyPlaylist404Ignored(t *testing.T) {
	for _, status := range []int{404, 401, 403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s, f := setup(t)
			f.status = status
			books, err := s.ABS.Playlist(context.Background(), "missing")
			if status == 404 {
				if err != nil || len(books) != 0 {
					t.Fatal(books, err)
				}
			} else if err == nil {
				t.Fatal("expected failure")
			}
			if _, err = s.ABS.Discover(context.Background(), s.Settings.Playlists); err == nil {
				t.Fatal("listing error ignored")
			}
		})
	}
}
func TestCacheSafetyAndProgress(t *testing.T) {
	s, _ := setup(t)
	manifest(t, s, "keep")
	manifest(t, s, "remove")
	if err := s.Cache.Prune(map[string]bool{"keep": true}); err != nil {
		t.Fatal(err)
	}
	if s.Cache.Manifest("keep") == nil || s.Cache.Manifest("remove") != nil {
		t.Fatal("prune")
	}
	if s.Cache.Manifest("../escape") != nil {
		t.Fatal("traversal")
	}
	s.Cache.Queue("book-a")
	s.Cache.Queue("book-a")
	if len(s.Cache.queue) != 1 || s.Cache.Status("book-a")["state"] != "queued" {
		t.Fatal("dedup")
	}
	s.Cache.running["book-a"] = true
	s.Cache.setGeneration("book-a", "rendering", 3, 12)
	v := s.Cache.Status("book-a")
	if v["state"] != "generating" || v["generation"].(Generation).Files != "3/12" {
		t.Fatal(v)
	}
	outside := filepath.Join(t.TempDir(), "outside.mp3")
	write(t, outside, []byte("secret"))
	dir := filepath.Join(s.Cache.root, "keep")
	if err := os.Remove(filepath.Join(dir, "0001.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "0001.mp3")); err != nil {
		t.Fatal(err)
	}
	if s.Cache.Manifest("keep") != nil {
		t.Fatal("symlink escape accepted")
	}
}
func TestPlan(t *testing.T) {
	got := plan("first", []unit{{10, 3605.5}}, 1800)
	want := []renderPart{{"first", 10, 1800}, {"first", 1810, 1800}, {"first", 3610, 5.5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}
func TestConversionAndDownload(t *testing.T) {
	for _, ext := range []string{".mp3", ".m4b"} {
		t.Run(ext, func(t *testing.T) {
			s, f := setup(t)
			data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "tone"+ext))
			if err != nil {
				t.Fatal(err)
			}
			f.audio = data
			f.extension = ext
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err = s.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if err = s.Cache.Build(ctx, "book-a"); err != nil {
				t.Fatal(err)
			}
			m := s.Cache.Manifest("book-a")
			want := 1
			if ext == ".m4b" {
				want = 2
			}
			if m == nil || len(m.Files) != want {
				t.Fatalf("manifest: %+v", m)
			}
			for _, file := range m.Files {
				path := s.Cache.File("book-a", file.ID)
				d, err := duration(ctx, path)
				if err != nil || d <= 0 {
					t.Fatal(d, err)
				}
				w := request(s, "/api/items/book-a/file/"+file.ID+"/download", "Bearer valid")
				actual, _ := os.ReadFile(path)
				if w.Code != 200 || !reflect.DeepEqual(actual, w.Body.Bytes()) {
					t.Fatal("download mismatch")
				}
				r := httptest.NewRequest("GET", "/api/items/book-a/file/"+file.ID+"/download", nil)
				r.Header.Set("Authorization", "Bearer valid")
				r.Header.Set("Range", "bytes=0-9")
				rw := httptest.NewRecorder()
				s.Handler().ServeHTTP(rw, r)
				if rw.Code != 206 || !reflect.DeepEqual(rw.Body.Bytes(), actual[:10]) {
					t.Fatal("range download failed")
				}
			}
			if err = s.Cache.Build(ctx, "book-a"); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			downloads := f.downloads
			f.mu.Unlock()
			if downloads != 1 {
				t.Fatal("ready item downloaded again")
			}
			entries, _ := os.ReadDir(filepath.Join(s.Cache.root, ".staging"))
			if len(entries) != 0 {
				t.Fatal("successful stage retained")
			}
			out, err := command(ctx, "ffprobe", "-v", "error", "-show_entries", "stream=codec_name,sample_rate,channels", "-of", "json", s.Cache.File("book-a", "chunk-001"))
			if err != nil || !strings.Contains(string(out), `"codec_name": "mp3"`) || !strings.Contains(string(out), `"sample_rate": "44100"`) {
				t.Fatalf("%s %v", out, err)
			}
		})
	}
}
func TestWorkerRetryAndShutdown(t *testing.T) {
	s, f := setup(t)
	f.audio = []byte("broken audio")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Cache.Worker(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop")
		}
	}()
	s.Cache.Queue("book-a")
	waitState(t, s, "book-a", "failed")
	if s.Cache.Manifest("book-a") != nil {
		t.Fatal("published partial output")
	}
	data, err := os.ReadFile("../../testdata/tone.mp3")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.audio = data
	f.extension = ".mp3"
	f.mu.Unlock()
	s.Cache.Queue("book-a")
	waitState(t, s, "book-a", "ready")
}
func waitState(t *testing.T, s *Server, id, state string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s.Cache.Status(id)["state"] == state {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("wanted %s, got %v", state, s.Cache.Status(id))
}
func TestMalformedPlaylistPreservesView(t *testing.T) {
	s, f := setup(t)
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status = 503
	f.mu.Unlock()
	if request(s, "/garmin/playlist-status/Adventure", "Bearer valid").Code != 502 {
		t.Fatal("upstream error not reported")
	}
	w := request(s, "/audiobookshelf/playlist?playlist_id=garmin-adventure", "Bearer valid")
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "book-b") {
		t.Fatal("lost snapshot")
	}
}

func TestMultipleSourcesUseWholeFiles(t *testing.T) {
	s, f := setup(t)
	data, err := os.ReadFile("../../testdata/tone.m4b")
	if err != nil {
		t.Fatal(err)
	}
	// Override only item metadata; the real client still downloads both sources over HTTP.
	upstream := s.ABS.URL
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/items/book-a" {
			item := bookData("book-a", ".m4b")
			media := obj(item["media"])
			media["audioFiles"] = []Object{
				{"ino": "first", "metadata": Object{"filename": "first.m4b"}},
				{"ino": "second", "metadata": Object{"filename": "second.m4b"}},
			}
			respond(w, 200, item)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), "GET", upstream+r.URL.Path, nil)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	s.ABS.URL = proxy.URL
	f.audio = data
	if err := s.Cache.Build(context.Background(), "book-a"); err != nil {
		t.Fatal(err)
	}
	m := s.Cache.Manifest("book-a")
	if m == nil || len(m.Files) != 2 {
		t.Fatalf("expected two full source files, got %+v", m)
	}
	for _, file := range m.Files {
		if file.Duration < 1.9 {
			t.Fatalf("source split at chapters: %+v", file)
		}
	}
}

func TestRemovedBookCannotPublishAfterPrune(t *testing.T) {
	s, f := setup(t)
	data, err := os.ReadFile("../../testdata/tone.mp3")
	if err != nil {
		t.Fatal(err)
	}
	f.audio = data
	f.extension = ".mp3"
	if err = s.Cache.Prune(map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if err = s.Cache.Build(context.Background(), "book-a"); err != nil {
		t.Fatal(err)
	}
	if s.Cache.Manifest("book-a") != nil {
		t.Fatal("removed book republished")
	}
}

func TestExpiredTokenRevalidated(t *testing.T) {
	s, f := setup(t)
	s.Settings.AuthCache = -time.Second
	for i := 0; i < 2; i++ {
		if request(s, "/audiobookshelf/playlists", "Bearer valid").Code != 200 {
			t.Fatal("auth")
		}
	}
	f.mu.Lock()
	calls := f.authCalls
	f.mu.Unlock()
	if calls != 2 {
		t.Fatal("expired auth reused")
	}
}

func TestRunStops(t *testing.T) {
	s, f := setup(t)
	f.items = map[string][]string{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("background tasks failed to stop")
	}
}

func TestPlaylistLookupByNameOrID(t *testing.T) {
	for _, name := range []string{"Garmin", "Adventure Books"} {
		for _, lookup := range []string{"name", "normalized-name", "id"} {
			t.Run(name+"/"+lookup, func(t *testing.T) {
				s, f := setup(t)
				groups, err := ParsePlaylists(name)
				if err != nil {
					t.Fatal(err)
				}
				s.Settings.Playlists = groups
				f.playlists = []Object{{"id": "first", "libraryId": "a", "name": name}}
				key := "playlist_name"
				selector := name
				if lookup == "normalized-name" {
					selector = "  " + strings.ToUpper(strings.ReplaceAll(name, " ", "   ")) + "  "
				}
				if lookup == "id" {
					key = "playlist_id"
					selector = groups[0].ID
				}
				// Exercise both the initial reconciliation and an existing cached snapshot.
				for i := 0; i < 2; i++ {
					w := request(s, "/audiobookshelf/playlist?"+key+"="+url.QueryEscape(selector), "Bearer valid")
					var books []Object
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &books) != nil || len(books) != 1 || books[0]["id"] != "book-a" {
						t.Fatalf("lookup failed: %d %s", w.Code, w.Body.String())
					}
				}
				w := request(s, "/audiobookshelf/playlists", "Bearer valid")
				var listed []Object
				if json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed) != 1 || listed[0]["id"] != groups[0].ID {
					t.Fatal("watch-facing ID changed", w.Body.String())
				}
			})
		}
	}
}

func TestPlaylistIDAndNameSelectIndependently(t *testing.T) {
	s, _ := setup(t)
	groups, err := ParsePlaylists("garmin-adventure,Adventure")
	if err != nil {
		t.Fatal(err)
	}
	s.Settings.Playlists = groups
	s.books = map[string][]Object{
		"garmin-garmin-adventure": {{"id": "name-match"}},
		"garmin-adventure":        {{"id": "id-match"}},
	}
	for key, want := range map[string]string{"playlist_id": "id-match", "playlist_name": "name-match"} {
		w := request(s, "/audiobookshelf/playlist?"+key+"=garmin-adventure", "Bearer valid")
		var books []Object
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &books) != nil || len(books) != 1 || books[0]["id"] != want {
			t.Fatalf("%s resolved incorrectly: %d %s", key, w.Code, w.Body.String())
		}
	}
}

func TestPlaylistSelectorValidation(t *testing.T) {
	s, _ := setup(t)
	for query, status := range map[string]int{
		"":                422,
		"?playlist_name=": 422,
		"?playlist_id=":   422,
		"?playlist_id=garmin-adventure&playlist_name=Adventure": 422,
		"?playlist_name=Adventure&playlist_name=Other":          422,
		"?playlist_id=garmin-adventure&playlist_id=other":       422,
		"?playlist_id=Adventure":                                404,
		"?playlist_name=missing":                                404,
	} {
		if w := request(s, "/audiobookshelf/playlist"+query, "Bearer valid"); w.Code != status {
			t.Fatalf("%s: got %d, want %d", query, w.Code, status)
		}
	}
}
