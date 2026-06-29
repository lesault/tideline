package server

import (
	"context"
	"encoding/json"
	"io"
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
	"github.com/lesault/tideline/internal/wallabag"
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

	s := New(st, auth.NewSessionManager(time.Hour), fetch.New(2*time.Second), wallabag.New(2*time.Second))
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

// mockWallabagServer issues a token then accepts entry creation, returning a
// fixed entry id.
func mockWallabagServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok123", "expires_in": 3600})
	})
	mux.HandleFunc("/api/entries.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": 9001})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (e *testEnv) saveWallabag(t *testing.T, baseURL string) {
	t.Helper()
	resp, err := e.client.PostForm(e.srv.URL+"/settings/wallabag", url.Values{
		"base_url": {baseURL}, "client_id": {"cid"}, "client_secret": {"sec"},
		"username": {"user"}, "password": {"pw"},
	})
	if err != nil {
		t.Fatalf("save wallabag: %v", err)
	}
	resp.Body.Close()
}

func TestUpdateDefaultTTLViaSettings(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "ttl@example.com", "password1")

	resp, err := e.client.PostForm(e.srv.URL+"/settings/ttl", url.Values{"days": {"3"}})
	if err != nil {
		t.Fatalf("ttl post: %v", err)
	}
	resp.Body.Close()

	u, _ := e.st.UserByID(context.Background(), 1)
	if u.DefaultTTLDays != 3 {
		t.Fatalf("default TTL = %d, want 3", u.DefaultTTLDays)
	}

	// New captures now expire ~3 days out, not 14.
	e.captureID(t, "https://example.com/x")
	links, _ := e.st.ListInbox(context.Background(), 1)
	got := time.Until(links[0].TTLExpiresAt).Hours()
	if got < 60 || got > 80 { // ~72h, allowing slack
		t.Fatalf("new link TTL ~%.0fh, want ~72h", got)
	}
}

func TestUpdateDefaultTTLRejectsNonsense(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "bad@example.com", "password1")
	for _, bad := range []string{"0", "-5", "abc", "100000"} {
		resp, _ := e.client.PostForm(e.srv.URL+"/settings/ttl", url.Values{"days": {bad}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("days=%q should be 400, got %d", bad, resp.StatusCode)
		}
	}
	u, _ := e.st.UserByID(context.Background(), 1)
	if u.DefaultTTLDays != 14 {
		t.Fatalf("default TTL should stay 14 after bad input, got %d", u.DefaultTTLDays)
	}
}

func TestArchivePushesToWallabag(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "wb@example.com", "password1")
	wb := mockWallabagServer(t)
	e.saveWallabag(t, wb.URL)
	id := e.captureID(t, "https://example.com/keep-me")

	resp, err := e.client.PostForm(e.srv.URL+"/links/1/archive", url.Values{})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	resp.Body.Close()

	got, _ := e.st.LinkByID(context.Background(), id)
	if got.Status != store.StatusArchived {
		t.Fatalf("status = %q, want archived", got.Status)
	}
	if got.WallabagEntryID == nil || *got.WallabagEntryID != 9001 {
		t.Fatalf("entry id not recorded: %+v", got.WallabagEntryID)
	}
}

func TestArchiveWithoutWallabagRedirectsToSettings(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "nowb@example.com", "password1")
	id := e.captureID(t, "https://example.com/x")

	// Don't follow the redirect, so we can assert where it points.
	noRedirect := *e.client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.PostForm(e.srv.URL+"/links/1/archive", url.Values{})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings" {
		t.Fatalf("want 303 → /settings, got %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	got, _ := e.st.LinkByID(context.Background(), id)
	if got.Status == store.StatusArchived {
		t.Fatal("link should not be archived without a Wallabag connection")
	}
}

func TestArchiveFailureKeepsLink(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "fail@example.com", "password1")
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer down.Close()
	e.saveWallabag(t, down.URL)
	id := e.captureID(t, "https://example.com/x")

	resp, err := e.client.PostForm(e.srv.URL+"/links/1/archive", url.Values{})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	resp.Body.Close()

	got, _ := e.st.LinkByID(context.Background(), id)
	if got.Status == store.StatusArchived {
		t.Fatal("a failed push must not mark the link archived")
	}
}

// dueLink inserts an inbox link already in the DueSoon window for user 1.
func (e *testEnv) seedDueLink(t *testing.T, url string) {
	t.Helper()
	now := time.Now().UTC()
	_, err := e.st.CreateLink(context.Background(), store.Link{
		UserID: 1, URL: url, Domain: "example.com",
		CreatedAt: now.Add(-13 * 24 * time.Hour), TTLExpiresAt: now.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("seed due link: %v", err)
	}
}

func TestCaptureViaCaptureToken(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "tok@example.com", "password1")
	raw, _, _ := e.st.CreateAPIToken(context.Background(), 1, store.ScopeCapture, "ext")

	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/links", strings.NewReader(`{"url":"https://example.com/from-ext"}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req) // no cookie jar — token only
	if err != nil {
		t.Fatalf("token capture: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	links, _ := e.st.ListInbox(context.Background(), 1)
	if len(links) != 1 || links[0].URL != "https://example.com/from-ext" {
		t.Fatalf("link not captured via token: %v", links)
	}
}

func TestCaptureRejectedByFeedToken(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "f@example.com", "password1")
	raw, _, _ := e.st.CreateAPIToken(context.Background(), 1, store.ScopeFeed, "reader")

	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/links", strings.NewReader(`{"url":"https://example.com/x"}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("feed token capturing should be 403, got %d", resp.StatusCode)
	}
}

func TestCountReportsDueItems(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "c@example.com", "password1")
	e.captureID(t, "https://example.com/fresh") // fresh, not due
	e.seedDueLink(t, "https://example.com/due") // due

	resp, err := e.client.Get(e.srv.URL + "/api/count")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Count int `json:"count"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Count != 1 {
		t.Fatalf("count = %d, want 1 (only the due link)", out.Count)
	}
}

func TestDueFeedListsDueItems(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "feed@example.com", "password1")
	e.seedDueLink(t, "https://example.com/due-article")
	raw, _, _ := e.st.CreateAPIToken(context.Background(), 1, store.ScopeFeed, "reader")

	resp, err := http.Get(e.srv.URL + "/feed/due?token=" + raw)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "rss") {
		t.Fatalf("content-type = %q, want rss", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "https://example.com/due-article") {
		t.Fatalf("feed missing the due link:\n%s", body)
	}
}

func TestDueFeedRequiresFeedScope(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "wrongscope@example.com", "password1")
	raw, _, _ := e.st.CreateAPIToken(context.Background(), 1, store.ScopeCapture, "ext")

	resp, err := http.Get(e.srv.URL + "/feed/due?token=" + raw)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("capture token on feed should be 401, got %d", resp.StatusCode)
	}
}

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
