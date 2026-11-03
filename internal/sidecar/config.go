package sidecar

import (
	"crypto/sha256"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var slugSeparators = regexp.MustCompile(`[^a-z0-9]+`)
var simplePlaylistName = regexp.MustCompile(`^[a-z0-9]+(?:[ -][a-z0-9]+)*$`)

type Playlist struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Sources []string `json:"source_playlist_ids"`
}
type Settings struct {
	ABSURL, Token, CacheDir, Bitrate string
	PollInterval, AuthCache          time.Duration
	StagingRetention                 time.Duration
	ChunkMinutes                     int
	Playlists                        []Playlist
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// ParsePlaylists reads one CSV record and gives every normalized name a stable ID.
func ParsePlaylists(value string) ([]Playlist, error) {
	reader := csv.NewReader(strings.NewReader(value))
	reader.TrimLeadingSpace = true
	fields, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("PLAYLIST_NAMES must be a non-empty CSV list: %w", err)
	}
	if _, err := reader.Read(); err != io.EOF {
		return nil, fmt.Errorf("PLAYLIST_NAMES must contain one CSV record")
	}
	playlists := []Playlist{}
	seen := map[string]bool{}
	ids := map[string]string{}
	for _, field := range fields {
		name := strings.Join(strings.Fields(field), " ")
		key := normalize(name)
		if key == "" {
			return nil, fmt.Errorf("PLAYLIST_NAMES cannot contain empty names")
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		slug := strings.Trim(slugSeparators.ReplaceAllString(key, "-"), "-")
		// Simple ASCII names preserve the established garmin-adventure convention.
		// Non-ASCII/punctuated names use a deterministic hash to avoid slug collisions.
		if slug == "" || !simplePlaylistName.MatchString(key) {
			hash := sha256.Sum256([]byte(key))
			if len(slug) > 48 {
				slug = slug[:48]
			}
			slug = strings.Trim(slug, "-") + fmt.Sprintf("-%x", hash[:8])
			slug = strings.TrimLeft(slug, "-")
		}
		id := "garmin-" + slug
		if previous, ok := ids[id]; ok {
			return nil, fmt.Errorf("playlist names %q and %q produce the same ID", previous, name)
		}
		ids[id] = name
		playlists = append(playlists, Playlist{ID: id, Name: name, Sources: []string{}})
	}
	return playlists, nil
}

func LoadSettings() (Settings, error) {
	var s Settings
	var err error
	s.Playlists, err = ParsePlaylists(os.Getenv("PLAYLIST_NAMES"))
	if err != nil {
		return s, err
	}

	s.ABSURL = strings.TrimRight(os.Getenv("ABS_URL"), "/")
	s.Token = os.Getenv("ABS_TOKEN")
	if file := os.Getenv("ABS_TOKEN_FILE"); file != "" {
		if s.Token != "" {
			return s, fmt.Errorf("Set only one of ABS_TOKEN or ABS_TOKEN_FILE")
		}
		data, err := os.ReadFile(file)
		if err != nil || !utf8.Valid(data) {
			return s, fmt.Errorf("ABS_TOKEN_FILE must point to a readable UTF-8 token file")
		}
		s.Token = strings.TrimSpace(string(data))
	}
	if s.ABSURL == "" || s.Token == "" {
		return s, fmt.Errorf("ABS_URL and either ABS_TOKEN or a non-empty ABS_TOKEN_FILE are required")
	}
	s.CacheDir = env("CACHE_DIR", "/app/cache")
	s.Bitrate = env("CACHE_BITRATE", "96k")
	poll, auth, retention := 300, 300, 86400
	values := []struct {
		key      string
		fallback int
		target   *int
	}{
		{"CACHE_CHUNK_MINUTES", 30, &s.ChunkMinutes},
		{"POLL_INTERVAL_SECONDS", 300, &poll},
		{"AUTH_CACHE_SECONDS", 300, &auth},
		{"STAGING_RETENTION_SECONDS", 86400, &retention},
	}
	for _, v := range values {
		n, e := strconv.Atoi(env(v.key, strconv.Itoa(v.fallback)))
		if e != nil || n < 1 || n > 100000000 {
			return s, fmt.Errorf("%s must be a positive integer within supported range", v.key)
		}
		*v.target = n
	}
	s.PollInterval = time.Duration(poll) * time.Second
	s.AuthCache = time.Duration(auth) * time.Second
	s.StagingRetention = time.Duration(retention) * time.Second
	return s, nil
}
