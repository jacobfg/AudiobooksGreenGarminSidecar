package sidecar

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SyncStatus struct {
	Socket        string     `json:"socket"`
	LastConnected *time.Time `json:"last_socket_connected_at"`
	LastEventAt   *time.Time `json:"last_socket_event_at"`
	LastEvent     string     `json:"last_socket_event,omitempty"`
	LastSuccess   time.Time  `json:"last_successful_refresh"`
	LastError     string     `json:"last_error,omitempty"`
}
type Server struct {
	events     chan struct{}
	syncStatus SyncStatus

	Settings Settings
	ABS      *ABS
	Cache    *Cache
	mu       sync.RWMutex
	refresh  sync.Mutex
	books    map[string][]Object
	tokens   map[[32]byte]time.Time
	sources  map[string][]string
}

func NewServer(s Settings) (*Server, error) {
	a := NewABS(s.ABSURL, s.Token)
	c, err := NewCache(s, a)
	if err != nil {
		return nil, err
	}
	return &Server{events: make(chan struct{}, 1), syncStatus: SyncStatus{Socket: "disconnected"}, Settings: s, ABS: a, Cache: c, books: map[string][]Object{}, tokens: map[[32]byte]time.Time{}}, nil
}
func (s *Server) notify() {
	select {
	case s.events <- struct{}{}:
	default:
	}
}
func (s *Server) synchronization() SyncStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.syncStatus
}
func (s *Server) socketState(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncStatus.Socket = state
	if state == "connected" {
		now := time.Now()
		s.syncStatus.LastConnected = &now
	}
}
func (s *Server) socketEvent(name string) {
	if name != "init" {
		now := time.Now()
		s.mu.Lock()
		s.syncStatus.LastEvent = name
		s.syncStatus.LastEventAt = &now
		s.mu.Unlock()
	}
	s.notify()
}
func (s *Server) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.Cache.Worker(ctx) }()
	go func() {
		defer wg.Done()
		s.ABS.events(ctx, s.socketEvent, s.socketState)
	}()
	defer wg.Wait()
	refresh := func() {
		if err := s.Cache.CleanupStaging(time.Now()); err != nil {
			slog.Error("Staging cleanup failed", "error", err)
		}
		if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			slog.Error("ABS playlist reconciliation failed", "error", err)
		}
	}
	refresh()
	ticker := time.NewTicker(s.Settings.PollInterval)
	defer ticker.Stop()
	var debounce *time.Timer
	var wake <-chan time.Time
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		case <-s.events:
			if wake == nil {
				debounce = time.NewTimer(500 * time.Millisecond)
				wake = debounce.C
			}
		case <-wake:
			wake = nil
			// Drain notifications already represented by this refresh. Events arriving
			// during HTTP reconciliation remain queued for a subsequent refresh.
			select {
			case <-s.events:
			default:
			}
			refresh()
		}
	}
}
func (s *Server) Reconcile(ctx context.Context) (err error) {
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			s.syncStatus.LastError = "Audiobookshelf refresh failed"
		} else {
			s.syncStatus.LastSuccess = time.Now()
			s.syncStatus.LastError = ""
		}
	}()

	s.refresh.Lock()
	defer s.refresh.Unlock()
	resolved, err := s.ABS.Discover(ctx, s.Settings.Playlists)
	if err != nil {
		return err
	}
	refreshed := map[string][]Object{}
	keep := map[string]bool{}
	desired := map[string]string{}
	for _, p := range s.Settings.Playlists {
		books := []Object{}
		seen := map[string]bool{}
		for _, id := range resolved[p.ID] {
			items, err := s.ABS.Playlist(ctx, id)
			if err != nil {
				return err
			}
			for _, book := range items {
				id := str(book["id"])
				if !seen[id] {
					books = append(books, book)
					seen[id] = true
					keep[id] = true
				}
			}
		}
		refreshed[p.ID] = books
	}
	// Fetch source metadata once per managed book so polling also detects audio
	// changes missed while the socket was disconnected.
	for id := range keep {
		item, e := s.ABS.Item(ctx, id)
		if e != nil {
			return e
		}
		desired[id] = sourceFingerprint(item, s.Settings)
	}
	s.Cache.SyncSources(desired)
	s.mu.Lock()
	s.books = refreshed
	s.sources = resolved
	s.mu.Unlock()
	return s.Cache.Prune(keep)
}
func (s *Server) group(id string) *Playlist {
	for _, p := range s.Settings.Playlists {
		if p.ID == id {
			return &p
		}
	}
	return nil
}
func (s *Server) groupByName(name string) *Playlist {
	for _, p := range s.Settings.Playlists {
		if normalize(p.Name) == normalize(name) {
			return &p
		}
	}
	return nil
}
func (s *Server) snapshot(id string) ([]Object, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.books[id]
	return b, ok
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	respond(w, status, Object{"detail": message})
}
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if !strings.HasPrefix(token, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(token, "Bearer ")) == "" {
			fail(w, 401, "Bearer token required")
			return
		}
		key := sha256.Sum256([]byte(token))
		now := time.Now()
		s.mu.RLock()
		expires := s.tokens[key]
		s.mu.RUnlock()
		if !expires.After(now) {
			if _, err := s.ABS.json(r.Context(), "/api/me", token, false); err != nil {
				fail(w, 401, "Invalid Audiobookshelf token")
				return
			}
			s.mu.Lock()
			for k, expiry := range s.tokens {
				if !expiry.After(now) {
					delete(s.tokens, k)
				}
			}
			if len(s.tokens) >= 4096 {
				clear(s.tokens)
			}
			s.tokens[key] = now.Add(s.Settings.AuthCache)
			s.mu.Unlock()
		}
		next(w, r)
	}
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, Object{"status": "ok"}) })
	mux.HandleFunc("GET /audiobookshelf/playlists", s.auth(s.playlists))
	mux.HandleFunc("GET /audiobookshelf/playlist", s.auth(s.playlist))
	mux.HandleFunc("GET /audiobookshelf/book", s.auth(s.book))
	mux.HandleFunc("GET /garmin/playlist-status/{name}", s.auth(s.status))
	mux.HandleFunc("GET /garmin/status", s.auth(func(w http.ResponseWriter, r *http.Request) { respond(w, 200, s.synchronization()) }))
	mux.HandleFunc("GET /api/items/{book}/file/{file}/download", s.auth(s.download))
	return mux
}
func (s *Server) playlists(w http.ResponseWriter, r *http.Request) {
	items := []Object{}
	for _, p := range s.Settings.Playlists {
		items = append(items, Object{"id": p.ID, "name": p.Name, "libraryId": "garmin-sidecar"})
	}
	respond(w, 200, items)
}
func (s *Server) playlist(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Has("playlist_id") == q.Has("playlist_name") {
		fail(w, 422, "Provide exactly one of playlist_id or playlist_name")
		return
	}
	key := "playlist_id"
	if q.Has("playlist_name") {
		key = "playlist_name"
	}
	if len(q[key]) != 1 || strings.TrimSpace(q.Get(key)) == "" {
		fail(w, 422, key+" must contain one non-empty value")
		return
	}
	var group *Playlist
	if key == "playlist_id" {
		group = s.group(q.Get(key))
	} else {
		group = s.groupByName(q.Get(key))
	}
	if group == nil {
		fail(w, 404, "Unknown virtual playlist")
		return
	}
	id := group.ID
	books, ok := s.snapshot(id)
	if !ok {
		if err := s.Reconcile(r.Context()); err != nil {
			s.upstreamError(w, err)
			return
		}
		books, _ = s.snapshot(id)
	}
	items := []Object{}
	for _, b := range books {
		items = append(items, summary(b))
	}
	respond(w, 200, items)
}
func (s *Server) upstreamError(w http.ResponseWriter, err error) {
	slog.Error("ABS request failed", "error", err)
	fail(w, 502, "Audiobookshelf request failed")
}
func (s *Server) book(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !q.Has("book_id") {
		fail(w, 422, "book_id is required")
		return
	}
	id := q.Get("book_id")
	skip, limit := 0, 0
	for name, target := range map[string]*int{"skip": &skip, "limit": &limit} {
		if q.Has(name) {
			n, err := strconv.Atoi(q.Get(name))
			if err != nil {
				fail(w, 422, name+" must be an integer")
				return
			}
			*target = n
		}
	}
	result := Object{"files": []File{}, "skip": skip, "limit": limit, "total": 0}
	s.mu.RLock()
	found := false
	for _, books := range s.books {
		for _, b := range books {
			if str(b["id"]) == id {
				found = true
			}
		}
	}
	s.mu.RUnlock()
	if found {
		m := s.Cache.Manifest(id)
		if m == nil {
			s.Cache.Queue(id)
		} else {
			result["total"] = len(m.Files)
			start := max(0, min(skip, len(m.Files)))
			end := len(m.Files)
			if limit > 0 && limit < end-start {
				end = start + limit
			}
			files := []File{}
			for _, f := range m.Files[start:end] {
				f.Path = ""
				files = append(files, f)
			}
			result["files"] = files
		}
	}
	respond(w, 200, result)
}
func formatHMS(seconds float64) string {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		seconds = 0
	}
	n := int64(seconds)
	return fmt.Sprintf("%d:%02d:%02d", n/3600, n/60%60, n%60)
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	var group *Playlist
	for _, p := range s.Settings.Playlists {
		if strings.EqualFold(p.Name, r.PathValue("name")) {
			group = &p
			break
		}
	}
	if group == nil {
		fail(w, 404, "Unknown virtual playlist name")
		return
	}
	if err := s.Reconcile(r.Context()); err != nil {
		s.upstreamError(w, err)
		return
	}
	books, _ := s.snapshot(group.ID)
	s.mu.RLock()
	group.Sources = append([]string{}, s.sources[group.ID]...)
	s.mu.RUnlock()
	result := []Object{}
	for _, b := range books {
		id := str(b["id"])
		item, err := s.ABS.Item(r.Context(), id)
		if err != nil {
			s.upstreamError(w, err)
			return
		}
		progress, err := s.ABS.Progress(r.Context(), id)
		if err != nil {
			s.upstreamError(w, err)
			return
		}
		sources := []Object{}
		duration := 0.
		for _, v := range arr(obj(item["media"])["audioFiles"]) {
			f := obj(v)
			d := number(f["duration"])
			ino := ""
			if f["ino"] != nil {
				ino = fmt.Sprint(f["ino"])
			}
			sources = append(sources, Object{"id": ino, "filename": str(obj(f["metadata"])["filename"]), "duration_seconds": d, "duration": formatHMS(d)})
			duration += d
		}
		current := number(progress["currentTime"])
		cache := s.Cache.Status(id)
		cache["duration"] = formatHMS(number(cache["duration_seconds"]))
		row := summary(item)
		row["duration_seconds"] = duration
		row["duration"] = formatHMS(duration)
		row["position_seconds"] = current
		row["position"] = formatHMS(current)
		row["progress"] = number(progress["progress"])
		row["source_files"] = sources
		row["cache"] = cache
		result = append(result, row)
	}
	respond(w, 200, Object{"playlist": group, "book_count": len(books), "books": result, "sync": s.synchronization()})
}
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	file, name, err := s.Cache.Open(r.PathValue("book"), r.PathValue("file"))
	if err != nil {
		fail(w, 404, "Cached Garmin media not found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		fail(w, 404, "Cached Garmin media not found")
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeContent(w, r, name, info.ModTime(), file)
}
