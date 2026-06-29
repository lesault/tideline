package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lesault/tideline/internal/auth"
	"github.com/lesault/tideline/internal/fetch"
	"github.com/lesault/tideline/internal/store"
)

// testEnv spins a full HTTP server backed by a temp DB, plus an upstream page
// server whose HTML the fetcher reads, and a cookie-jar client.
type testEnv struct {
	srv      *httptest.Server
	upstream *httptest.Server
	client   *http.Client
	st       *store.Store
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Captured Page</title>
			<meta name="description" content="A page worth reading."></head></html>`))
	}))
	t.Cleanup(upstream.Close)

	s := New(st, auth.NewSessionManager(time.Hour), fetch.New(2*time.Second))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	return &testEnv{srv: srv, upstream: upstream, client: &http.Client{Jar: jar}, st: st}
}

func (e *testEnv) register(t *testing.T, email, pw string) {
	t.Helper()
	resp, err := e.client.PostForm(e.srv.URL+"/register", url.Values{"email": {email}, "password": {pw}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("register status %d", resp.StatusCode)
	}
}

func (e *testEnv) captureJSON(t *testing.T, link string) *http.Response {
	t.Helper()
	resp, err := e.client.Post(e.srv.URL+"/api/links", "application/json",
		strings.NewReader(`{"url":`+jsonString(link)+`}`))
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	return resp
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func (e *testEnv) inboxURLs(t *testing.T) []string {
	t.Helper()
	resp, err := e.client.Get(e.srv.URL + "/api/links")
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("inbox status %d", resp.StatusCode)
	}
	var out []struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode inbox: %v", err)
	}
	urls := make([]string, len(out))
	for i, l := range out {
		urls[i] = l.URL
	}
	return urls
}

func TestCaptureAppearsInInbox(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "alice@example.com", "secretpw")

	resp := e.captureJSON(t, "https://example.com/article")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("capture status = %d, want 201", resp.StatusCode)
	}

	urls := e.inboxURLs(t)
	if len(urls) != 1 || urls[0] != "https://example.com/article" {
		t.Fatalf("inbox = %v, want the captured url", urls)
	}
}

func TestInboxRequiresAuth(t *testing.T) {
	e := newTestEnv(t)
	// No registration → no session cookie.
	resp, err := e.client.Get(e.srv.URL + "/api/links")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestCaptureIsScopedPerUser(t *testing.T) {
	alice := newTestEnv(t)
	alice.register(t, "alice@example.com", "password1")
	alice.captureJSON(t, "https://alice.example/x").Body.Close()

	// A second independent client (Mallory) against the same server.
	jar, _ := cookiejar.New(nil)
	mallory := &testEnv{srv: alice.srv, client: &http.Client{Jar: jar}, st: alice.st}
	mallory.register(t, "mallory@example.com", "password1")
	if urls := mallory.inboxURLs(t); len(urls) != 0 {
		t.Fatalf("mallory should see no links, got %v", urls)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "bob@example.com", "rightpassword")

	jar, _ := cookiejar.New(nil)
	fresh := &http.Client{Jar: jar}
	resp, err := fresh.PostForm(e.srv.URL+"/login", url.Values{"email": {"bob@example.com"}, "password": {"wrongpassword"}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("login with wrong password should not redirect to a session")
	}
}

func (e *testEnv) captureID(t *testing.T, link string) int64 {
	t.Helper()
	resp := e.captureJSON(t, link)
	defer resp.Body.Close()
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode capture: %v", err)
	}
	return out.ID
}

func TestTriageMovesCardToBoard(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "tri@example.com", "password1")
	id := e.captureID(t, "https://example.com/read-me")
	cats, _ := e.st.ListCategories(context.Background(), 1)

	resp, err := e.client.PostForm(e.srv.URL+"/triage/1",
		url.Values{"category_id": {fmtInt(cats[0].ID)}, "next_step": {"read"}})
	if err != nil {
		t.Fatalf("triage post: %v", err)
	}
	resp.Body.Close()

	board, _ := e.st.ListBoard(context.Background(), 1)
	if len(board) != 1 || board[0].ID != id {
		t.Fatalf("expected 1 card on board, got %v", board)
	}
	if board[0].Status != store.StatusTriaged || board[0].BoardColumn != store.ColReviewing {
		t.Fatalf("card not triaged into Reviewing: %+v", board[0])
	}
}

func TestDropFromInbox(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "drop@example.com", "password1")
	e.captureID(t, "https://example.com/junk")

	resp, err := e.client.PostForm(e.srv.URL+"/links/1/drop", url.Values{})
	if err != nil {
		t.Fatalf("drop post: %v", err)
	}
	resp.Body.Close()

	if urls := e.inboxURLs(t); len(urls) != 0 {
		t.Fatalf("dropped link should leave the inbox, got %v", urls)
	}
}

func TestMoveCardEndpoint(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "mv@example.com", "password1")
	e.captureID(t, "https://example.com/x")
	cats, _ := e.st.ListCategories(context.Background(), 1)
	e.client.PostForm(e.srv.URL+"/triage/1", url.Values{"category_id": {fmtInt(cats[0].ID)}, "next_step": {"read"}})

	resp, err := e.client.PostForm(e.srv.URL+"/cards/1/move", url.Values{"column": {store.ColNext}, "position": {"0"}})
	if err != nil {
		t.Fatalf("move post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("move status = %d, want 204", resp.StatusCode)
	}

	board, _ := e.st.ListBoard(context.Background(), 1)
	if len(board) != 1 || board[0].BoardColumn != store.ColNext {
		t.Fatalf("card not moved to Next: %v", board)
	}
}

func TestMoveCardRejectsBadColumn(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "badcol@example.com", "password1")
	e.captureID(t, "https://example.com/x")
	cats, _ := e.st.ListCategories(context.Background(), 1)
	e.client.PostForm(e.srv.URL+"/triage/1", url.Values{"category_id": {fmtInt(cats[0].ID)}, "next_step": {"read"}})

	resp, err := e.client.PostForm(e.srv.URL+"/cards/1/move", url.Values{"column": {"Bogus"}, "position": {"0"}})
	if err != nil {
		t.Fatalf("move post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad column status = %d, want 400", resp.StatusCode)
	}
}

func TestAddCategory(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "cat@example.com", "password1")

	resp, err := e.client.PostForm(e.srv.URL+"/categories", url.Values{"name": {"Cooking"}})
	if err != nil {
		t.Fatalf("category post: %v", err)
	}
	resp.Body.Close()

	cats, _ := e.st.ListCategories(context.Background(), 1)
	found := false
	for _, c := range cats {
		if c.Name == "Cooking" {
			found = true
		}
	}
	if !found {
		t.Fatalf("new category not created; got %v", cats)
	}
}

func fmtInt(i int64) string { return strconv.FormatInt(i, 10) }

func TestAsyncEnrichmentFillsTitle(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "c@example.com", "password1")
	e.captureJSON(t, e.upstream.URL).Body.Close()

	// Enrichment runs in a goroutine; poll briefly for it to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		links, _ := e.st.ListInbox(context.Background(), 1)
		if len(links) == 1 && links[0].FetchStatus == "ok" {
			if links[0].Title != "Captured Page" {
				t.Fatalf("title = %q, want enriched", links[0].Title)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("metadata enrichment did not complete in time")
}
