package sidecar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
)

func TestDiscoveryAcrossLibraries(t *testing.T) {
	s, f := setup(t)
	f.playlists = []Object{{"id": "first", "libraryId": "a", "name": " adventure "}, {"id": "second", "libraryId": "b", "name": "ADVENTURE"}, {"id": "duplicate", "libraryId": "a", "name": "Adventure"}, {"id": "other", "libraryId": "a", "name": "Other"}}
	groups := append(s.Settings.Playlists, Playlist{ID: "missing", Name: "Missing"})
	got, err := s.ABS.Discover(context.Background(), groups)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["garmin-adventure"], []string{"duplicate", "first", "second"}) || len(got["missing"]) != 0 {
		t.Fatal(got)
	}
}
func TestDiscoveryMissingRecreatedAndFailures(t *testing.T) {
	s, f := setup(t)
	manifest(t, s, "book-a")
	manifest(t, s, "book-b")
	ctx := context.Background()
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []any{nil, []Object{{"id": "bad", "libraryId": "a"}}} {
		f.playlists = bad
		if err := s.Reconcile(ctx); err == nil {
			t.Fatal("malformed listing accepted")
		}
		books, _ := s.snapshot("garmin-adventure")
		if len(books) != 2 || s.Cache.Manifest("book-b") == nil {
			t.Fatal("failed refresh destroyed snapshot")
		}
	}
	f.playlists = []Object{{"id": "first", "libraryId": "a", "name": "Adventure"}}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Cache.Manifest("book-b") != nil {
		t.Fatal("removed source retained")
	}
	f.playlists = []Object{{"id": "first", "libraryId": "a", "name": "Adventure"}, {"id": "second", "libraryId": "b", "name": "adventure"}}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	books, _ := s.snapshot("garmin-adventure")
	if len(books) != 2 {
		t.Fatal(books)
	}
	f.playlists = []Object{}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if request(s, "/audiobookshelf/playlists", "Bearer valid").Code != 200 {
		t.Fatal("empty configured playlist missing")
	}
}

func TestDiscoveryPaginationAndIncompleteSnapshots(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/libraries" {
					respond(w, 200, Object{"libraries": []Object{{"id": "a"}}})
					return
				}
				if r.URL.Query().Get("limit") != "100" {
					t.Error("missing page limit")
				}
				page, err := strconv.Atoi(r.URL.Query().Get("page"))
				if err != nil {
					t.Error(err)
					return
				}
				entries := []Object{}
				for i := page * 100; i < min((page+1)*100, 101); i++ {
					entries = append(entries, Object{"id": fmt.Sprintf("p-%03d", i), "name": "Adventure", "libraryId": "a"})
				}
				if broken && page == 0 {
					entries = entries[:99]
				}
				respond(w, 200, Object{"results": entries, "total": 101})
			}))
			defer server.Close()
			got, err := NewABS(server.URL, "token").Discover(context.Background(), []Playlist{{ID: "adventure", Name: "Adventure"}})
			if broken {
				if err == nil {
					t.Fatal("accepted incomplete listing")
				}
				return
			}
			if err != nil || len(got["adventure"]) != 101 {
				t.Fatal(got, err)
			}
		})
	}
}
