package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
func (a *ABS) Resolve(ctx context.Context, sources []string) (map[string]string, error) {
	result := map[string]string{}
	if len(sources) == 0 {
		return result, nil
	}
	libs, err := a.json(ctx, "/api/libraries", "", false)
	if err != nil {
		return nil, err
	}
	pls, err := a.json(ctx, "/api/playlists", "", false)
	if err != nil {
		return nil, err
	}
	for _, v := range []struct {
		data   Object
		key    string
		fields []string
	}{{libs, "libraries", []string{"id", "name"}}, {pls, "playlists", []string{"id", "name", "libraryId"}}} {
		entries, ok := v.data[v.key].([]any)
		if !ok {
			return nil, fmt.Errorf("ABS returned an invalid library or playlist listing")
		}
		for _, e := range entries {
			for _, f := range v.fields {
				if _, ok := obj(e)[f].(string); !ok {
					return nil, fmt.Errorf("ABS returned an invalid library or playlist listing")
				}
			}
		}
	}
	for _, source := range sources {
		parts := strings.SplitN(source, "/", 2)
		libID := ""
		count := 0
		for _, v := range arr(libs["libraries"]) {
			l := obj(v)
			if normalize(str(l["name"])) == normalize(parts[0]) {
				libID = str(l["id"])
				count++
			}
		}
		if count > 1 {
			return nil, fmt.Errorf("Ambiguous ABS library name in %q; use a playlist ID", source)
		}
		count = 0
		for _, v := range arr(pls["playlists"]) {
			p := obj(v)
			if libID != "" && str(p["libraryId"]) == libID && normalize(str(p["name"])) == normalize(parts[1]) {
				result[source] = str(p["id"])
				count++
			}
		}
		if count > 1 {
			return nil, fmt.Errorf("Ambiguous ABS playlist name in %q; use a playlist ID", source)
		}
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
