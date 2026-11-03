package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type Object = map[string]any

func obj(v any) Object {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return Object{}
}
func arr(v any) []any  { a, _ := v.([]any); return a }
func str(v any) string { s, _ := v.(string); return s }
func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}
func summary(book Object) Object {
	m := obj(obj(book["media"])["metadata"])
	author := ""
	if a := arr(m["authors"]); len(a) > 0 {
		author = str(obj(a[0])["name"])
	}
	return Object{"id": book["id"], "title": str(m["title"]), "author": author}
}

type ABS struct {
	URL, Token string
	Client     *http.Client
}

func NewABS(base, token string) *ABS {
	return &ABS{strings.TrimRight(base, "/"), token, &http.Client{Timeout: 30 * time.Minute}}
}
func (a *ABS) get(ctx context.Context, path, token string) (*http.Response, error) {
	if token == "" {
		token = a.Token
	}
	req, err := http.NewRequestWithContext(ctx, "GET", a.URL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(strings.TrimPrefix(token, "Bearer ")))
	return a.Client.Do(req)
}
func (a *ABS) json(ctx context.Context, path, token string, missing bool) (Object, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := a.get(ctx, path, token)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if missing && r.StatusCode == 404 {
		return nil, nil
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return nil, fmt.Errorf("ABS GET %s failed (%d)", path, r.StatusCode)
	}
	var result Object
	err = json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&result)
	if err == nil && result == nil {
		err = fmt.Errorf("ABS GET %s did not return an object", path)
	}
	return result, err
}
func normalize(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// Discover searches each accessible library using the supported per-library API.
// Any incomplete/malformed listing fails the whole snapshot, preventing pruning.
func (a *ABS) Discover(ctx context.Context, groups []Playlist) (map[string][]string, error) {
	payload, err := a.json(ctx, "/api/libraries", "", false)
	if err != nil {
		return nil, err
	}
	libraries, ok := payload["libraries"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid ABS library listing")
	}
	ids := []string{}
	for _, v := range libraries {
		l := obj(v)
		id, ok := l["id"].(string)
		if !ok || id == "" {
			return nil, fmt.Errorf("invalid ABS library ID")
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := map[string][]string{}
	wanted := map[string]string{}
	for _, p := range groups {
		wanted[normalize(p.Name)] = p.ID
		result[p.ID] = []string{}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		for page := 0; ; page++ {
			path := fmt.Sprintf("/api/libraries/%s/playlists?limit=100&page=%d", url.PathEscape(id), page)
			payload, err := a.json(ctx, path, "", false)
			if err != nil {
				return nil, err
			}
			entries, ok := payload["results"].([]any)
			total, valid := payload["total"].(float64)
			if !ok || !valid || total < 0 || total != float64(int(total)) || page >= 10000 {
				return nil, fmt.Errorf("invalid ABS playlist listing")
			}
			if len(entries) < min(100, max(0, int(total)-page*100)) {
				return nil, fmt.Errorf("incomplete ABS playlist listing")
			}
			for _, v := range entries {
				p := obj(v)
				pid, pok := p["id"].(string)
				name, nok := p["name"].(string)
				if !pok || !nok || pid == "" || str(p["libraryId"]) != id {
					return nil, fmt.Errorf("invalid ABS playlist entry")
				}
				if group, match := wanted[normalize(name)]; match && !seen[pid] {
					result[group] = append(result[group], pid)
					seen[pid] = true
				}
			}
			if float64((page+1)*100) >= total {
				break
			}
		}
	}
	for id := range result {
		sort.Strings(result[id])
	}
	return result, nil
}
func (a *ABS) Playlist(ctx context.Context, id string) ([]Object, error) {
	p, err := a.json(ctx, "/api/playlists/"+url.PathEscape(id), "", true)
	if err != nil {
		return nil, err
	}
	result := []Object{}
	if p == nil {
		return result, nil
	}
	items, ok := p["items"].([]any)
	if !ok {
		return nil, fmt.Errorf("ABS returned invalid playlist items")
	}
	for _, v := range items {
		b := obj(obj(v)["libraryItem"])
		if id, ok := b["id"].(string); ok && id != "" {
			result = append(result, b)
		}
	}
	return result, nil
}
func (a *ABS) Item(ctx context.Context, id string) (Object, error) {
	return a.json(ctx, "/api/items/"+url.PathEscape(id), "", false)
}
func (a *ABS) Progress(ctx context.Context, id string) (Object, error) {
	return a.json(ctx, "/api/me/progress/"+url.PathEscape(id), "", true)
}
func (a *ABS) Download(ctx context.Context, id, file, target string) error {
	r, err := a.get(ctx, "/api/items/"+url.PathEscape(id)+"/file/"+url.PathEscape(file)+"/download", "")
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return fmt.Errorf("ABS download failed (%d)", r.StatusCode)
	}
	f, err := os.Create(target)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r.Body)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
