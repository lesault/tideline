package wallabag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mockWallabag stands in for a Wallabag instance: it issues a token then
// accepts an authenticated entry creation.
func mockWallabag(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var captured []string
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("grant_type") != "password" || r.FormValue("client_id") == "" {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok123", "refresh_token": "ref456", "expires_in": 3600,
		})
	})
	mux.HandleFunc("/api/entries.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok123" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.ParseForm()
		captured = append(captured, r.FormValue("url"))
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"id": 4242, "url": r.FormValue("url")})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &captured
}

func testConfig(base string) Config {
	return Config{BaseURL: base, ClientID: "cid", ClientSecret: "secret", Username: "user", Password: "pw"}
}

func TestArchiveCreatesEntry(t *testing.T) {
	srv, captured := mockWallabag(t)
	c := New(3 * time.Second)

	id, err := c.Archive(context.Background(), testConfig(srv.URL), "https://example.com/read")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if id != 4242 {
		t.Fatalf("entry id = %d, want 4242", id)
	}
	if len(*captured) != 1 || (*captured)[0] != "https://example.com/read" {
		t.Fatalf("entry url not sent correctly: %v", *captured)
	}
}

func TestArchiveTrimsTrailingSlashInBaseURL(t *testing.T) {
	srv, _ := mockWallabag(t)
	c := New(3 * time.Second)
	cfg := testConfig(srv.URL + "/") // user pasted a trailing slash
	if _, err := c.Archive(context.Background(), cfg, "https://example.com/x"); err != nil {
		t.Fatalf("Archive with trailing slash: %v", err)
	}
}

func TestArchiveFailsOnBadCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid_grant", http.StatusBadRequest)
	}))
	defer srv.Close()
	c := New(3 * time.Second)
	if _, err := c.Archive(context.Background(), testConfig(srv.URL), "https://example.com/x"); err == nil {
		t.Fatal("expected an error when authentication fails")
	}
}

func TestArchiveFailsWhenEntryRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok123", "expires_in": 3600})
	})
	mux.HandleFunc("/api/entries.json", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(3 * time.Second)
	_, err := c.Archive(context.Background(), testConfig(srv.URL), "https://example.com/x")
	if err == nil || !strings.Contains(err.Error(), "entry") {
		t.Fatalf("expected an entry-creation error, got %v", err)
	}
}
