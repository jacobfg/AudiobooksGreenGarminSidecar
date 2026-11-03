package sidecar

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type Playlist struct {
	ID      string   `yaml:"id" json:"id"`
	Name    string   `yaml:"name" json:"name"`
	Sources []string `yaml:"source_playlist_ids" json:"source_playlist_ids"`
}
type Settings struct {
	ABSURL, Token, CacheDir, Bitrate string
	PollInterval, AuthCache          time.Duration
	StagingRetention                 time.Duration
	ChunkMinutes                     int
	Playlists                        []Playlist `yaml:"virtual_playlists"`
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
func LoadSettings(path string) (Settings, error) {
	var s Settings
	if path == "" {
		path = env("SIDECAR_CONFIG", "/app/config.yaml")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	// Decode nodes first: YAML's string coercion must not accept numeric IDs.
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil {
		return s, err
	}
	if err = yaml.Unmarshal(data, &s); err != nil {
		return s, err
	}
	if len(s.Playlists) == 0 {
		return s, fmt.Errorf("config must contain a non-empty virtual_playlists list")
	}
	var checkStrings func(*yaml.Node) error
	checkStrings = func(n *yaml.Node) error {
		if n.Kind == yaml.MappingNode {
			for i := 0; i < len(n.Content); i += 2 {
				k, v := n.Content[i].Value, n.Content[i+1]
				if k == "id" || k == "name" {
					if v.Tag != "!!str" {
						return fmt.Errorf("playlist %s must be a string", k)
					}
				}
				if k == "source_playlist_ids" {
					if v.Kind != yaml.SequenceNode {
						return fmt.Errorf("invalid source playlist list")
					}
					for _, x := range v.Content {
						if x.Tag != "!!str" {
							return fmt.Errorf("invalid source playlist ID")
						}
					}
				}
			}
		}
		for _, child := range n.Content {
			if err := checkStrings(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err = checkStrings(&doc); err != nil {
		return s, err
	}
	seen := map[string]bool{}
	for i := range s.Playlists {
		p := &s.Playlists[i]
		if !safeID.MatchString(p.ID) || seen[p.ID] {
			return s, fmt.Errorf("virtual playlist IDs must be unique letters, digits, _ or -")
		}
		seen[p.ID] = true
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" || len(p.Sources) == 0 {
			return s, fmt.Errorf("virtual playlist %s requires name and source_playlist_ids", p.ID)
		}
		for j, source := range p.Sources {
			source = strings.TrimSpace(source)
			for _, part := range strings.SplitN(source, "/", 2) {
				if strings.TrimSpace(part) == "" {
					return s, fmt.Errorf("invalid source playlist ID or library/playlist name")
				}
			}
			p.Sources[j] = source
		}
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
