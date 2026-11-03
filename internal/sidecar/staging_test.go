package sidecar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func agePath(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}
func stageDir(t *testing.T, s *Server, name string, when time.Time) string {
	t.Helper()
	path := filepath.Join(s.Cache.root, ".staging", name)
	if err := os.MkdirAll(filepath.Join(path, "sources"), 0755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(path, "sources", "partial.mp3"), []byte("partial"))
	agePath(t, path, when)
	return path
}
func assertExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Lstat(path)
	if want && err != nil {
		t.Fatalf("expected %s to survive: %v", path, err)
	}
	if !want && !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed: %v", path, err)
	}
}

func TestStagingCleanupRetentionAndScope(t *testing.T) {
	s, _ := setup(t)
	now := time.Now()
	old := stageDir(t, s, "book-a-old", now.Add(-25*time.Hour))
	boundary := stageDir(t, s, "book-a-boundary", now.Add(-24*time.Hour))
	recent := stageDir(t, s, "book-a-recent", now.Add(-time.Hour))
	// A second attempt for the same book must not keep an earlier expired attempt.
	active, err := s.Cache.beginStage("book-a")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Cache.finishStage(active)
	agePath(t, active, now.Add(-48*time.Hour))
	manifest(t, s, "published")
	outside := t.TempDir()
	write(t, filepath.Join(outside, "keep.mp3"), []byte("keep"))
	link := filepath.Join(s.Cache.root, ".staging", "outside-link")
	if err = os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(s.Cache.root, ".staging", "note.txt")
	write(t, note, []byte("diagnosis"))
	agePath(t, note, now.Add(-48*time.Hour))
	if err = s.Cache.CleanupStaging(now); err != nil {
		t.Fatal(err)
	}
	assertExists(t, old, false)
	assertExists(t, boundary, false)
	for _, path := range []string{recent, active, link, note, filepath.Join(outside, "keep.mp3")} {
		assertExists(t, path, true)
	}
	if s.Cache.Manifest("published") == nil {
		t.Fatal("published media touched by staging cleanup")
	}
}

func TestActiveBuildSurvivesCleanupAndRetainsFailure(t *testing.T) {
	s, _ := setup(t)
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/items/book-a" {
			respond(w, 200, bookData("book-a", ".mp3"))
			return
		}
		close(started)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	s.ABS.URL = upstream.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Cache.Build(ctx, "book-a") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
	paths, err := filepath.Glob(filepath.Join(s.Cache.root, ".staging", "book-a-*"))
	if err != nil || len(paths) != 1 {
		t.Fatal(paths, err)
	}
	stage := paths[0]
	agePath(t, stage, time.Now().Add(-48*time.Hour))
	if err = s.Cache.CleanupStaging(time.Now()); err != nil {
		t.Fatal(err)
	}
	assertExists(t, stage, true)
	stopped := time.Now()
	cancel()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("cancelled download succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("build did not stop")
	}
	info, err := os.Stat(stage)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Before(stopped.Add(-time.Second)) {
		t.Fatal("failed attempt did not receive a fresh retention timestamp")
	}
	if err = s.Cache.CleanupStaging(info.ModTime().Add(23 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertExists(t, stage, true)
	if err = s.Cache.CleanupStaging(info.ModTime().Add(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertExists(t, stage, false)
}

func TestPollingCleansStagingWhileABSUnavailable(t *testing.T) {
	s, f := setup(t)
	f.mu.Lock()
	f.status = 503
	f.mu.Unlock()
	s.Settings.PollInterval = 20 * time.Millisecond
	s.Cache.settings.StagingRetention = time.Hour
	first := stageDir(t, s, "previous-process", time.Now().Add(-2*time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	}()
	waitRemoved := func(path string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("poll did not clean %s", path)
	}
	waitRemoved(first)
	// Created after startup: proves that later polls clean too, despite failed refreshes.
	second := stageDir(t, s, "later-failed-attempt", time.Now().Add(-2*time.Hour))
	waitRemoved(second)
}

func TestStagingRetentionConfiguration(t *testing.T) {
	configEnv(t)
	t.Setenv("ABS_TOKEN", "token")
	s, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.StagingRetention != 24*time.Hour {
		t.Fatal(s.StagingRetention)
	}
	t.Setenv("STAGING_RETENTION_SECONDS", "3600")
	s, err = LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if s.StagingRetention != time.Hour {
		t.Fatal(s.StagingRetention)
	}
}
