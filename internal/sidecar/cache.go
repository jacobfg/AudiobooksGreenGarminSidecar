package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type File struct {
	ID       string  `json:"id"`
	Path     string  `json:"path,omitempty"`
	Filename string  `json:"filename"`
	Duration float64 `json:"duration"`
}
type Manifest struct {
	Files []File `json:"files"`
}
type Generation struct {
	Phase   string `json:"phase"`
	Current int    `json:"current"`
	Total   int    `json:"total"`
	Files   string `json:"files"`
}
type Cache struct {
	// Serialize staging creation, registration, and cleanup so active work is never removed.
	stagingMu    sync.Mutex
	activeStages map[string]bool

	// Publication and pruning must not race: a removed book may still be encoding.
	publication sync.Mutex
	keep        map[string]bool

	settings   Settings
	abs        *ABS
	root       string
	mu         sync.Mutex
	pending    map[string]bool
	running    map[string]bool
	errors     map[string]string
	generation map[string]Generation
	queue      []string
	wake       chan struct{}
}

func NewCache(s Settings, a *ABS) (*Cache, error) {
	if s.StagingRetention == 0 {
		s.StagingRetention = 24 * time.Hour
	}
	root, err := filepath.Abs(s.CacheDir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Join(root, ".staging"), 0755); err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	return &Cache{activeStages: map[string]bool{}, settings: s, abs: a, root: root, pending: map[string]bool{}, running: map[string]bool{}, errors: map[string]string{}, generation: map[string]Generation{}, wake: make(chan struct{}, 1)}, nil
}
func (c *Cache) directory(id string) string {
	if !safeID.MatchString(id) {
		return ""
	}
	p := filepath.Join(c.root, id)
	if info, err := os.Lstat(p); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return ""
	}
	return p
}
func readManifest(dir string) *Manifest {
	if dir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil
	}
	var m Manifest
	if json.Unmarshal(data, &m) != nil || len(m.Files) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		if !safeID.MatchString(f.ID) || seen[f.ID] || f.Path == "" || filepath.Base(f.Path) != f.Path || f.Filename == "" || math.IsNaN(f.Duration) || math.IsInf(f.Duration, 0) || f.Duration < 0 {
			return nil
		}
		path, err := filepath.EvalSymlinks(filepath.Join(dir, f.Path))
		if err != nil || filepath.Dir(path) != dir {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		seen[f.ID] = true
	}
	return &m
}
func (c *Cache) Manifest(id string) *Manifest { return readManifest(c.directory(id)) }
func (c *Cache) File(id, file string) string {
	if m := c.Manifest(id); m != nil {
		for _, f := range m.Files {
			if f.ID == file {
				return filepath.Join(c.directory(id), f.Path)
			}
		}
	}
	return ""
}
func (c *Cache) Queue(id string) {
	if c.directory(id) == "" || c.Manifest(id) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending[id] {
		return
	}
	c.pending[id] = true
	c.generation[id] = Generation{"queued", 0, 0, "0/0"}
	c.queue = append(c.queue, id)
	select {
	case c.wake <- struct{}{}:
	default:
	}
}
func (c *Cache) setGeneration(id, phase string, current, total int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation[id] = Generation{phase, current, total, fmt.Sprintf("%d/%d", current, total)}
}
func (c *Cache) Worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		c.mu.Lock()
		if len(c.queue) == 0 {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
			}
			continue
		}
		id := c.queue[0]
		c.queue = c.queue[1:]
		c.running[id] = true
		c.mu.Unlock()
		err := c.Build(ctx, id)
		c.mu.Lock()
		delete(c.pending, id)
		delete(c.running, id)
		if err != nil {
			c.errors[id] = err.Error()
			slog.Error("Garmin conversion failed", "item", id, "error", err)
		} else {
			delete(c.errors, id)
		}
		c.mu.Unlock()
	}
}
func (c *Cache) Status(id string) Object {
	m := c.Manifest(id)
	c.mu.Lock()
	defer c.mu.Unlock()
	var gen any
	if g, ok := c.generation[id]; ok {
		gen = g
	}
	if m != nil {
		duration := 0.
		for _, f := range m.Files {
			duration += f.Duration
		}
		r := Object{"state": "ready", "exists": true, "file_count": len(m.Files), "duration_seconds": duration}
		if gen != nil {
			r["generation"] = gen
		}
		return r
	}
	state := "missing"
	var last any
	if e, ok := c.errors[id]; ok {
		state = "failed"
		last = e
	}
	if c.pending[id] {
		state = "queued"
	}
	if c.running[id] {
		state = "generating"
	}
	return Object{"state": state, "exists": false, "file_count": 0, "duration_seconds": 0., "generation": gen, "last_error": last}
}
func (c *Cache) Prune(keep map[string]bool) error {
	c.publication.Lock()
	defer c.publication.Unlock()
	c.keep = make(map[string]bool, len(keep))
	for id, present := range keep {
		c.keep[id] = present
	}

	entries, err := os.ReadDir(c.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && safeID.MatchString(entry.Name()) && !keep[entry.Name()] {
			if err = os.RemoveAll(filepath.Join(c.root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%s: %s", name, strings.TrimSpace(string(e.Stderr)))
		}
		return nil, err
	}
	return out, nil
}
func duration(ctx context.Context, path string) (float64, error) {
	out, err := command(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
	if err != nil {
		return 0, err
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || math.IsInf(d, 0) || math.IsNaN(d) || d <= 0 {
		return 0, fmt.Errorf("invalid audio duration")
	}
	return d, nil
}

type unit struct{ Start, Duration float64 }

func chapters(ctx context.Context, path string) ([]unit, error) {
	out, err := command(ctx, "ffprobe", "-v", "error", "-show_chapters", "-of", "json", path)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Chapters []struct {
			Start string `json:"start_time"`
			End   string `json:"end_time"`
		}
	}
	if err = json.Unmarshal(out, &payload); err != nil {
		return nil, err
	}
	units := []unit{}
	for _, ch := range payload.Chapters {
		start, e1 := strconv.ParseFloat(ch.Start, 64)
		end, e2 := strconv.ParseFloat(ch.End, 64)
		if e1 == nil && e2 == nil && start >= 0 && end > start && !math.IsInf(end, 0) {
			units = append(units, unit{start, end - start})
		}
	}
	if len(units) == 0 {
		d, e := duration(ctx, path)
		if e != nil {
			return nil, e
		}
		units = append(units, unit{0, d})
	}
	return units, nil
}

type renderPart struct {
	Source          string
	Start, Duration float64
}

func plan(source string, units []unit, seconds float64) []renderPart {
	parts := []renderPart{}
	for _, u := range units {
		end := u.Start + u.Duration
		for pos := u.Start; pos < end; {
			n := math.Min(seconds, end-pos)
			parts = append(parts, renderPart{source, pos, n})
			pos += n
		}
	}
	return parts
}
func (c *Cache) Build(ctx context.Context, id string) error {
	dir := c.directory(id)
	if dir == "" || c.Manifest(id) != nil {
		return nil
	}
	stage, err := c.beginStage(id)
	if err != nil {
		return err
	}
	defer c.finishStage(stage)

	sources, rendered := filepath.Join(stage, "sources"), filepath.Join(stage, "rendered")
	if err = os.Mkdir(sources, 0755); err != nil {
		return err
	}
	if err = os.Mkdir(rendered, 0755); err != nil {
		return err
	}
	item, err := c.abs.Item(ctx, id)
	if err != nil {
		return err
	}
	files := arr(obj(item["media"])["audioFiles"])
	if len(files) == 0 {
		return fmt.Errorf("ABS item has no audio files")
	}
	for _, v := range files {
		f := obj(v)
		ext := strings.ToLower(filepath.Ext(str(obj(f["metadata"])["filename"])))
		if (ext != ".m4b" && ext != ".mp3") || f["ino"] == nil {
			return fmt.Errorf("only M4B and MP3 source media are supported")
		}
	}
	downloaded := []string{}
	for i, v := range files {
		f := obj(v)
		target := filepath.Join(sources, fmt.Sprintf("source-%04d%s", i+1, strings.ToLower(filepath.Ext(str(obj(f["metadata"])["filename"])))))
		c.setGeneration(id, "downloading", i, len(files))
		if err = c.abs.Download(ctx, id, fmt.Sprint(f["ino"]), target); err != nil {
			return err
		}
		downloaded = append(downloaded, target)
		c.setGeneration(id, "downloading", i+1, len(files))
	}
	parts := []renderPart{}
	for _, source := range downloaded {
		var units []unit
		if len(downloaded) > 1 {
			d, e := duration(ctx, source)
			if e != nil {
				return e
			}
			units = []unit{{0, d}}
		} else {
			units, err = chapters(ctx, source)
			if err != nil {
				return err
			}
		}
		parts = append(parts, plan(source, units, float64(c.settings.ChunkMinutes)*60)...)
	}
	if len(parts) == 0 {
		return fmt.Errorf("conversion produced no files")
	}
	m := Manifest{Files: []File{}}
	for i, p := range parts {
		c.setGeneration(id, "rendering", i, len(parts))
		name := fmt.Sprintf("%04d.mp3", i+1)
		target := filepath.Join(rendered, name)
		_, err = command(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-ss", fmt.Sprintf("%.3f", p.Start), "-i", p.Source, "-t", fmt.Sprintf("%.3f", p.Duration), "-vn", "-map_metadata", "-1", "-map_chapters", "-1", "-ar", "44100", "-ac", "2", "-c:a", "libmp3lame", "-b:a", c.settings.Bitrate, target)
		if err != nil {
			return err
		}
		d, e := duration(ctx, target)
		if e != nil {
			return e
		}
		m.Files = append(m.Files, File{fmt.Sprintf("chunk-%03d", i+1), name, name, d})
		c.setGeneration(id, "rendering", i+1, len(parts))
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(rendered, "manifest.json"), data, 0644); err != nil {
		return err
	}
	if readManifest(rendered) == nil {
		return fmt.Errorf("generated media manifest is invalid")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	c.publication.Lock()
	defer c.publication.Unlock()
	if c.keep != nil && !c.keep[id] {
		return os.RemoveAll(stage)
	}
	if err = os.RemoveAll(dir); err != nil {
		return err
	}
	if err = os.Rename(rendered, dir); err != nil {
		return err
	}
	return os.RemoveAll(stage)
}

// Stable ordering makes background conversion deterministic across polls.
func (c *Cache) QueueAll(ids map[string]bool) {
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, id := range keys {
		c.Queue(id)
	}
}

// CleanupStaging removes expired, inactive attempt directories, including attempts
// left by a previous process. It never follows symlinks or touches published media.
func (c *Cache) CleanupStaging(now time.Time) error {
	c.stagingMu.Lock()
	defer c.stagingMu.Unlock()
	root := filepath.Join(c.root, ".staging")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	cutoff := now.Add(-c.settings.StagingRetention)
	var failures []error
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if !entry.IsDir() || c.activeStages[path] {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (c *Cache) beginStage(id string) (string, error) {
	c.stagingMu.Lock()
	defer c.stagingMu.Unlock()
	stage, err := os.MkdirTemp(filepath.Join(c.root, ".staging"), id+"-")
	if err == nil {
		c.activeStages[stage] = true
	}
	return stage, err
}

func (c *Cache) finishStage(stage string) {
	c.stagingMu.Lock()
	defer c.stagingMu.Unlock()
	// Retain a failed/cancelled attempt for a full interval after it stops,
	// even if downloading or encoding took longer than the retention period.
	now := time.Now()
	if err := os.Chtimes(stage, now, now); err != nil && !os.IsNotExist(err) {
		slog.Warn("Could not update staging retention timestamp", "path", stage, "error", err)
	}
	delete(c.activeStages, stage)
}
