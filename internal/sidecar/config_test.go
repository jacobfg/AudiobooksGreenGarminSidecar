package sidecar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = "virtual_playlists:\n  - id: adventure\n    name: Adventure\n    source_playlist_ids: [' Library A/Adventure Books ', old-id]\n"

func configEnv(t *testing.T) string {
	t.Helper()
	for _, key := range []string{"ABS_TOKEN", "ABS_TOKEN_FILE"} {
		t.Setenv(key, "")
	}
	t.Setenv("ABS_URL", "http://abs/")
	t.Setenv("POLL_INTERVAL_SECONDS", "300")
	t.Setenv("STAGING_RETENTION_SECONDS", "86400")
	t.Setenv("AUTH_CACHE_SECONDS", "300")
	t.Setenv("CACHE_CHUNK_MINUTES", "30")
	path := filepath.Join(t.TempDir(), "config.yaml")
	write(t, path, []byte(validConfig))
	return path
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
			path := configEnv(t)
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
			s, err := LoadSettings(path)
			if kind == "environment" || kind == "file" {
				if err != nil {
					t.Fatal(err)
				}
				if s.Token != "token" || s.ABSURL != "http://abs" || s.Playlists[0].Sources[0] != "Library A/Adventure Books" || s.PollInterval != 300*time.Second {
					t.Fatalf("unexpected settings: %+v", s)
				}
			} else if err == nil {
				t.Fatal("expected invalid settings")
			}
		})
	}
}
func TestInvalidConfig(t *testing.T) {
	for _, source := range []string{"''", "'  '", "'/Adventure'", "'Library/  '", "123"} {
		t.Run(source, func(t *testing.T) {
			p := configEnv(t)
			t.Setenv("ABS_TOKEN", "token")
			write(t, p, []byte(strings.Replace(validConfig, "' Library A/Adventure Books ', old-id", source, 1)))
			if _, err := LoadSettings(p); err == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
	for _, key := range []string{"POLL_INTERVAL_SECONDS", "AUTH_CACHE_SECONDS", "CACHE_CHUNK_MINUTES", "STAGING_RETENTION_SECONDS"} {
		for _, value := range []string{"0", "-1", "abc"} {
			t.Run(key+value, func(t *testing.T) {
				p := configEnv(t)
				t.Setenv("ABS_TOKEN", "token")
				t.Setenv(key, value)
				if _, err := LoadSettings(p); err == nil {
					t.Fatal("invalid integer accepted")
				}
			})
		}
	}
}
func TestInvalidPlaylists(t *testing.T) {
	for _, contents := range []string{"", "virtual_playlists: []", "virtual_playlists: [123]", strings.Replace(validConfig, "id: adventure", "id: ../escape", 1), strings.Replace(validConfig, "name: Adventure", "name: ''", 1), strings.Replace(validConfig, "id: adventure", "id: 123", 1), validConfig + strings.TrimPrefix(validConfig, "virtual_playlists:\n")} {
		t.Run(contents, func(t *testing.T) {
			p := configEnv(t)
			t.Setenv("ABS_TOKEN", "token")
			write(t, p, []byte(contents))
			if _, err := LoadSettings(p); err == nil {
				t.Fatal("invalid playlist accepted")
			}
		})
	}
}
