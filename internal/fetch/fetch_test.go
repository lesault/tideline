package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchRetrievesAndParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Fetched Page</title>
			<meta name="description" content="From the server."></head></html>`))
	}))
	defer srv.Close()

	f := New(2 * time.Second)
	m, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if m.Title != "Fetched Page" || m.Excerpt != "From the server." {
		t.Fatalf("unexpected metadata: %+v", m)
	}
}

func TestFetchErrorsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	f := New(2 * time.Second)
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error for a 404 response")
	}
}
