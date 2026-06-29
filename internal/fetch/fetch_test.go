package fetch

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The parsing/transport tests hit a loopback httptest server, so they use the
// private-allowing fetcher; the SSRF guard is exercised separately below.

func TestFetchRetrievesAndParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Fetched Page</title>
			<meta name="description" content="From the server."></head></html>`))
	}))
	defer srv.Close()

	f := NewAllowingPrivate(2 * time.Second)
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

	f := NewAllowingPrivate(2 * time.Second)
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error for a 404 response")
	}
}

// TestSSRFGuardBlocksLoopback is the core regression test: the default fetcher
// (used for untrusted user URLs) must refuse to connect to a loopback address.
func TestSSRFGuardBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head><title>INTERNAL secret</title></head></html>`))
	}))
	defer srv.Close()

	f := New(2 * time.Second) // the secure constructor
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("SSRF guard should refuse to fetch a loopback URL")
	}
	if !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("expected an SSRF-guard error, got: %v", err)
	}
}

// TestSSRFGuardBlocksOnRedirect ensures a public-looking URL can't bounce the
// fetcher to an internal address via a redirect.
func TestSSRFGuardBlocksOnRedirect(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head><title>INTERNAL via redirect</title></head></html>`))
	}))
	defer internal.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer redirector.Close()

	// Even reaching the redirector is loopback here, so this asserts the guard
	// fires; the redirect target is loopback too. Both must be refused.
	f := New(2 * time.Second)
	if _, err := f.Fetch(context.Background(), redirector.URL); err == nil {
		t.Fatal("SSRF guard should refuse loopback (incl. redirect targets)")
	}
}

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", // loopback
		"10.0.0.5", "172.16.3.4", "192.168.1.1", "fc00::1", // private / ULA
		"169.254.169.254", "fe80::1", // link-local (incl. cloud metadata)
		"0.0.0.0", "::", // unspecified
		"224.0.0.1", // multicast
	}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("expected %s to be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"}
	for _, s := range allowed {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("expected %s to be allowed (public)", s)
		}
	}
}
