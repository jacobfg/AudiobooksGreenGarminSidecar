package sidecar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func configEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"ABS_TOKEN", "ABS_TOKEN_FILE"} {
		t.Setenv(key, "")
	}
	t.Setenv("ABS_URL", "http://abs/")
	t.Setenv("PLAYLIST_NAMES", " Adventure Books , Running")
	t.Setenv("POLL_INTERVAL_SECONDS", "300")
	t.Setenv("AUTH_CACHE_SECONDS", "300")
	t.Setenv("CACHE_CHUNK_MINUTES", "30")
	t.Setenv("STAGING_RETENTION_SECONDS", "86400")
}
func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}
func TestSettings(t *testing.T) {
	for _, kind := range []string{"environment", "file", "conflict", "empty", "whitespace", "missing-file", "directory", "invalid-utf8", "missing-token"} {
		t.Run(kind, func(t *testing.T) {
			configEnv(t)
			secret := filepath.Join(t.TempDir(), "secret")
			switch kind {
			case "environment":
				t.Setenv("ABS_TOKEN", "token")
			case "file":
				write(t, secret, []byte(" token\n"))
			case "conflict":
				t.Setenv("ABS_TOKEN", "token")
			case "empty":
				write(t, secret, nil)
			case "whitespace":
				write(t, secret, []byte(" \n\t"))
			case "directory":
				if err := os.Mkdir(secret, 0755); err != nil {
					t.Fatal(err)
				}
			case "invalid-utf8":
				write(t, secret, []byte{255})
			}
			if kind != "environment" && kind != "missing-token" {
				t.Setenv("ABS_TOKEN_FILE", secret)
			}
			s, err := LoadSettings()
			if kind == "environment" || kind == "file" {
				if err != nil {
					t.Fatal(err)
				}
				if s.Token != "token" || s.ABSURL != "http://abs" || s.Playlists[0].Name != "Adventure Books" || s.PollInterval != 300*time.Second {
					t.Fatalf("unexpected settings: %+v", s)
				}
			} else if err == nil {
				t.Fatal("expected invalid settings")
			}
		})
	}
}
func TestInvalidConfig(t *testing.T) {
	for _, key := range []string{"POLL_INTERVAL_SECONDS", "AUTH_CACHE_SECONDS", "CACHE_CHUNK_MINUTES", "STAGING_RETENTION_SECONDS"} {
		for _, value := range []string{"0", "-1", "abc"} {
			t.Run(key+value, func(t *testing.T) {
				configEnv(t)
				t.Setenv("ABS_TOKEN", "token")
				t.Setenv(key, value)
				if _, err := LoadSettings(); err == nil {
					t.Fatal("invalid integer accepted")
				}
			})
		}
	}
}
func TestPlaylistCSV(t *testing.T) {
	for _, value := range []string{"", " ", "Adventure,", ",Adventure", "Adventure,,Running", "\"bad", "Adventure\nRunning", "A B,A-B"} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParsePlaylists(value); err == nil {
				t.Fatalf("accepted %q", value)
			}
		})
	}
	playlists, err := ParsePlaylists(` Adventure , adventure, "History, Politics", 日本語, Running`)
	if err != nil {
		t.Fatal(err)
	}
	if len(playlists) != 4 || playlists[0].ID != "garmin-adventure" || playlists[1].Name != "History, Politics" {
		t.Fatal(playlists)
	}
	for _, p := range playlists {
		again, err := ParsePlaylists(strings.ToUpper(p.Name))
		if strings.Contains(p.Name, ",") {
			continue
		}
		if err != nil || again[0].ID != p.ID {
			t.Fatal("unstable ID", p)
		}
	}
}
