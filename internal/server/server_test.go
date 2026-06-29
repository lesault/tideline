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
	app      *Server
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

	// The test upstream is a loopback httptest server, so use the private-allowing
	// fetcher here; the SSRF guard on fetch.New is covered in the fetch package.
	s := New(st, auth.NewSessionManager(time.Hour), fetch.NewAllowingPrivate(2*time.Second), wallabag.New(2*time.Second))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	return &testEnv{srv: srv, upstream: upstream, client: &http.Client{Jar: jar}, st: st, app: s}
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

// get fetches an authenticated HTML path and returns its body as a string.
func (e *testEnv) get(t *testing.T, path string) string {
	t.Helper()
	resp, err := e.client.Get(e.srv.URL + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s status = %d, want 200", path, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

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

func TestAPIRejectsQueryToken(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "q@example.com", "password1")
	raw, _, _ := e.st.CreateAPIToken(context.Background(), 1, store.ScopeCapture, "ext")

	// Token in the query string must NOT authenticate the JSON API.
	resp, err := http.Get(e.srv.URL + "/api/count?token=" + raw)
	if err != nil {
		t.Fatalf("count via query: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query-token on /api/count = %d, want 401", resp.StatusCode)
	}

	// The same token in the Authorization header works.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/count", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("count via header: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("bearer-token on /api/count = %d, want 200", resp2.StatusCode)
	}
}

func TestSessionCookieSecureFlag(t *testing.T) {
	e := newTestEnv(t)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// Default over plain HTTP: no Secure flag (so it works on a LAN).
	resp, err := noFollow.PostForm(e.srv.URL+"/register", url.Values{"email": {"a@example.com"}, "password": {"password1"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	resp.Body.Close()
	if c := resp.Header.Get("Set-Cookie"); !strings.Contains(c, "tideline_session=") || strings.Contains(c, "Secure") {
		t.Fatalf("plain-HTTP cookie should not be Secure: %q", c)
	}

	// X-Forwarded-Proto: https (TLS-terminating proxy) → Secure.
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/login", strings.NewReader(url.Values{"email": {"a@example.com"}, "password": {"password1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp2, err := noFollow.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp2.Body.Close()
	if c := resp2.Header.Get("Set-Cookie"); !strings.Contains(c, "Secure") {
		t.Fatalf("HTTPS (via X-Forwarded-Proto) cookie should be Secure: %q", c)
	}
}

func TestEnrichmentIsBounded(t *testing.T) {
	e := newTestEnv(t)
	e.app.enrichSem = make(chan struct{}, 1) // capacity 1 for the test

	// Saturate the single slot — the next schedule must skip, not spawn.
	e.app.enrichSem <- struct{}{}
	if e.app.scheduleEnrich(1, e.upstream.URL, "x") {
		t.Fatal("scheduleEnrich should skip when at capacity")
	}
	<-e.app.enrichSem // free the slot
	if !e.app.scheduleEnrich(1, e.upstream.URL, "x") {
		t.Fatal("scheduleEnrich should run when a slot is free")
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newTestEnv(t)
	resp, err := e.client.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q; got %q", want, csp)
		}
	}
	// The CSP must not be loosened with unsafe-inline/eval for scripts.
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("script CSP should stay strict: %q", csp)
	}
}

func TestAssetsAreSelfHostedNotCDN(t *testing.T) {
	e := newTestEnv(t)
	// No page should pull executable/style assets from a third-party CDN.
	body := e.get(t, "/login")
	for _, bad := range []string{"unpkg.com", "googleapis.com", "gstatic.com", "jsdelivr"} {
		if strings.Contains(body, bad) {
			t.Fatalf("page references external CDN %q (should be self-hosted):\n%s", bad, body)
		}
	}
	if !strings.Contains(body, "/static/htmx.min.js") {
		t.Fatalf("expected self-hosted htmx reference, got:\n%s", body)
	}
	// The vendored assets are actually served (embedded).
	for _, path := range []string{"/static/htmx.min.js", "/static/fonts/fraunces-latin-wght-normal.woff2"} {
		resp, err := e.client.Get(e.srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
}

func TestLoginIsRateLimited(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "rl@example.com", "password1")
	e.app.loginLimiter = newRateLimiter(2, time.Minute) // tighten for the test

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	var last int
	for i := 0; i < 3; i++ {
		resp, err := c.PostForm(e.srv.URL+"/login", url.Values{"email": {"rl@example.com"}, "password": {"wrong"}})
		if err != nil {
			t.Fatalf("login %d: %v", i, err)
		}
		last = resp.StatusCode
		resp.Body.Close()
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("3rd login over the limit = %d, want 429", last)
	}
}

func TestRegistrationCanBeClosed(t *testing.T) {
	e := newTestEnv(t)
	e.app.openRegistration = false

	// POST is refused.
	resp, err := e.client.PostForm(e.srv.URL+"/register", url.Values{"email": {"x@example.com"}, "password": {"password1"}})
	if err != nil {
		t.Fatalf("register post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("register POST when closed = %d, want 403", resp.StatusCode)
	}
	// The page shows a "closed" message rather than the form.
	body := e.get(t, "/register")
	if !strings.Contains(body, "Registration closed") {
		t.Fatalf("register page when closed should say it's closed:\n%s", body)
	}
}

func TestLoginVerifiesPasswordEvenForUnknownUser(t *testing.T) {
	e := newTestEnv(t)
	if e.app.dummyHash == "" {
		t.Fatal("expected a dummy hash for timing equalization")
	}
	var calls int
	e.app.verifyPassword = func(hash, pw string) bool { calls++; return auth.VerifyPassword(hash, pw) }

	resp, err := e.client.PostForm(e.srv.URL+"/login", url.Values{"email": {"ghost@example.com"}, "password": {"whatever"}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown-user login = %d, want 401", resp.StatusCode)
	}
	// The password check must still run (against the dummy hash) — otherwise the
	// fast no-such-user path leaks valid emails by timing.
	if calls != 1 {
		t.Fatalf("verifyPassword called %d times for an unknown user, want 1", calls)
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
		url.Values{"category_id": {fmtInt(cats[0].ID)}, "next_step": {"schedule"}, "scheduled_for": {"2099-01-01"}})
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
	if board[0].ScheduledFor.IsZero() {
		t.Fatalf("scheduling should set ScheduledFor: %+v", board[0])
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
	e.client.PostForm(e.srv.URL+"/triage/1", url.Values{"category_id": {fmtInt(cats[0].ID)}, "next_step": {"review"}})

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
	e.client.PostForm(e.srv.URL+"/triage/1", url.Values{"category_id": {fmtInt(cats[0].ID)}, "next_step": {"review"}})

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

func TestTriageListShowsMultipleLinks(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "list@example.com", "password1")
	e.captureID(t, "https://example.com/one")
	e.captureID(t, "https://example.com/two")

	resp, err := e.client.Get(e.srv.URL + "/inbox")
	if err != nil {
		t.Fatalf("triage list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("triage list status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if !strings.Contains(html, "https://example.com/one") || !strings.Contains(html, "https://example.com/two") {
		t.Fatalf("triage list should show all inbox links:\n%s", html)
	}
}

func TestTriageFocusShowsOneCard(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "focus@example.com", "password1")
	e.captureID(t, "https://example.com/one")
	e.captureID(t, "https://example.com/two")

	resp, err := e.client.Get(e.srv.URL + "/triage/focus")
	if err != nil {
		t.Fatalf("triage focus: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("triage focus status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "triage-card") {
		t.Fatalf("focus view should show a single card:\n%s", body)
	}
}

func TestScheduleResurfacesInCount(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "sched@example.com", "password1")
	e.captureID(t, "https://example.com/later")

	// Schedule with a past date, so it is immediately "due" again.
	resp, err := e.client.PostForm(e.srv.URL+"/triage/1",
		url.Values{"next_step": {"schedule"}, "scheduled_for": {"2020-01-01"}})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	resp.Body.Close()

	cresp, err := e.client.Get(e.srv.URL + "/api/count")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	defer cresp.Body.Close()
	var out struct {
		Count int `json:"count"`
	}
	json.NewDecoder(cresp.Body).Decode(&out)
	if out.Count != 1 {
		t.Fatalf("count = %d, want 1 (past-scheduled link resurfaced)", out.Count)
	}
}

func TestTriageRejectsReferenceNextStep(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "ref@example.com", "password1")
	e.captureID(t, "https://example.com/ref")

	resp, err := e.client.PostForm(e.srv.URL+"/triage/1", url.Values{"next_step": {"reference"}})
	if err != nil {
		t.Fatalf("reference: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("next_step=reference should be 400, got %d", resp.StatusCode)
	}
}

func TestReviewThenReferencePromotesToLibrary(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "lib@example.com", "password1")
	id := e.captureID(t, "https://example.com/keep-forever")

	// Triage with the timing-only "review" step parks it on the board.
	resp, err := e.client.PostForm(e.srv.URL+"/triage/1", url.Values{"next_step": {"review"}})
	if err != nil {
		t.Fatalf("review triage: %v", err)
	}
	resp.Body.Close()

	// The Reference verdict promotes it into the reference library.
	resp, err = e.client.PostForm(e.srv.URL+"/links/1/reference", url.Values{})
	if err != nil {
		t.Fatalf("reference: %v", err)
	}
	resp.Body.Close()

	refs, _ := e.st.ListReference(context.Background(), 1, "")
	if len(refs) != 1 || refs[0].ID != id {
		t.Fatalf("expected the link in ListReference, got %+v", refs)
	}

	body := e.get(t, "/reef")
	if !strings.Contains(body, "https://example.com/keep-forever") {
		t.Fatalf("library should list the referenced link:\n%s", body)
	}
}

func TestNotesAreSearchableInLibrary(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "notes@example.com", "password1")
	e.captureID(t, "https://example.com/notable")
	e.client.PostForm(e.srv.URL+"/triage/1", url.Values{"next_step": {"review"}})
	e.client.PostForm(e.srv.URL+"/links/1/reference", url.Values{})

	resp, err := e.client.PostForm(e.srv.URL+"/links/1/notes", url.Values{"notes": {"remember this kubernetes trick"}})
	if err != nil {
		t.Fatalf("notes: %v", err)
	}
	resp.Body.Close()

	body := e.get(t, "/reef?q=kubernetes")
	if !strings.Contains(body, "https://example.com/notable") {
		t.Fatalf("library search by note word should find the link:\n%s", body)
	}
}

func TestLibraryRenders(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "render@example.com", "password1")
	e.captureID(t, "https://example.com/in-library")
	e.client.PostForm(e.srv.URL+"/triage/1", url.Values{"next_step": {"review"}})
	e.client.PostForm(e.srv.URL+"/links/1/reference", url.Values{})

	resp, err := e.client.Get(e.srv.URL + "/reef")
	if err != nil {
		t.Fatalf("library: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("library status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "https://example.com/in-library") {
		t.Fatalf("library page should show the referenced url:\n%s", body)
	}
}

// postHTMX submits a form with the HX-Request header set, simulating htmx.
func (e *testEnv) postHTMX(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("htmx post %s: %v", path, err)
	}
	return resp
}

func TestHTMXDropReturnsFragmentNotPage(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "hx@example.com", "password1")
	e.captureID(t, "https://example.com/x")

	resp := e.postHTMX(t, "/links/1/drop", url.Values{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("htmx drop status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if strings.Contains(html, "<html") {
		t.Fatalf("htmx response should be a fragment, not a full page:\n%s", html)
	}
	if !strings.Contains(html, "hx-swap-oob") || !strings.Contains(html, "inbox-count") {
		t.Fatalf("htmx fragment should carry the OOB inbox-count span:\n%s", html)
	}
	if urls := e.inboxURLs(t); len(urls) != 0 {
		t.Fatalf("dropped link should leave the inbox, got %v", urls)
	}
}

func TestNonHTMXDropStillRedirects(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "noh@example.com", "password1")
	e.captureID(t, "https://example.com/x")

	noRedirect := *e.client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.PostForm(e.srv.URL+"/links/1/drop", url.Values{})
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("non-htmx drop should 303 redirect, got %d", resp.StatusCode)
	}
}

func TestMergedNoteAndReferenceVerdict(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "merge@example.com", "password1")
	id := e.captureID(t, "https://example.com/merge")
	e.client.PostForm(e.srv.URL+"/triage/1", url.Values{"next_step": {"review"}})

	// One POST to the reference verdict carrying a note must persist both.
	resp, err := e.client.PostForm(e.srv.URL+"/links/1/reference", url.Values{"notes": {"merged note kept"}})
	if err != nil {
		t.Fatalf("reference+note: %v", err)
	}
	resp.Body.Close()

	got, _ := e.st.LinkByID(context.Background(), id)
	if got.Status != store.StatusReference {
		t.Fatalf("status = %q, want reference", got.Status)
	}
	body := e.get(t, "/reef")
	if !strings.Contains(body, "https://example.com/merge") || !strings.Contains(body, "merged note kept") {
		t.Fatalf("library should show the referenced link with its note:\n%s", body)
	}
}

func TestRestoreToInboxFromDrop(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "restore@example.com", "password1")
	e.captureID(t, "https://example.com/back")
	e.client.PostForm(e.srv.URL+"/links/1/drop", url.Values{})
	if urls := e.inboxURLs(t); len(urls) != 0 {
		t.Fatalf("link should be dropped first, got %v", urls)
	}

	resp, err := e.client.PostForm(e.srv.URL+"/links/1/restore", url.Values{"status": {"inbox"}})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	resp.Body.Close()

	links, _ := e.st.ListInbox(context.Background(), 1)
	if len(links) != 1 {
		t.Fatalf("restored link should be back in the inbox, got %v", links)
	}
	if !links[0].TTLExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("restored link should have a future TTL, got %v", links[0].TTLExpiresAt)
	}
}

func TestRestoreToBoard(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "rboard@example.com", "password1")
	id := e.captureID(t, "https://example.com/onboard")
	e.client.PostForm(e.srv.URL+"/triage/1", url.Values{"next_step": {"review"}})
	e.client.PostForm(e.srv.URL+"/links/1/reference", url.Values{})

	resp, err := e.client.PostForm(e.srv.URL+"/links/1/restore",
		url.Values{"status": {"triaged"}, "column": {store.ColReviewing}})
	if err != nil {
		t.Fatalf("restore to board: %v", err)
	}
	resp.Body.Close()

	board, _ := e.st.ListBoard(context.Background(), 1)
	if len(board) != 1 || board[0].ID != id || board[0].BoardColumn != store.ColReviewing {
		t.Fatalf("link should be back on the board in Reviewing, got %+v", board)
	}
}

func TestFlotsamRestoreReturnsToInbox(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "flot@example.com", "password1")
	now := time.Now().UTC()
	_, err := e.st.CreateLink(context.Background(), store.Link{
		UserID: 1, URL: "https://example.com/old", Domain: "example.com",
		CreatedAt: now.Add(-20 * 24 * time.Hour), TTLExpiresAt: now.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.st.SweepExpired(context.Background(), now); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	flot, _ := e.st.ListByStatus(context.Background(), 1, store.StatusFlotsam)
	if len(flot) != 1 {
		t.Fatalf("link should be in flotsam after sweep, got %v", flot)
	}

	resp, err := e.client.PostForm(e.srv.URL+"/links/1/restore", url.Values{"status": {"inbox"}})
	if err != nil {
		t.Fatalf("flotsam restore: %v", err)
	}
	resp.Body.Close()

	links, _ := e.st.ListInbox(context.Background(), 1)
	if len(links) != 1 {
		t.Fatalf("swept link should return to the inbox, got %v", links)
	}
}

func TestSaveThemePersists(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "theme@example.com", "password1")

	resp, err := e.client.PostForm(e.srv.URL+"/settings/theme", url.Values{"theme": {"deep"}})
	if err != nil {
		t.Fatalf("theme post: %v", err)
	}
	resp.Body.Close()

	u, _ := e.st.UserByID(context.Background(), 1)
	if u.Theme != "deep" {
		t.Fatalf("theme = %q, want deep", u.Theme)
	}
}

func TestSaveThemeRejectsInvalid(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "badtheme@example.com", "password1")

	resp, err := e.client.PostForm(e.srv.URL+"/settings/theme", url.Values{"theme": {"neon"}})
	if err != nil {
		t.Fatalf("theme post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid theme status = %d, want 400", resp.StatusCode)
	}
	u, _ := e.st.UserByID(context.Background(), 1)
	if u.Theme != "" {
		t.Fatalf("theme should stay empty after bad input, got %q", u.Theme)
	}
}

func TestPageReflectsStoredTheme(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "t2@example.com", "password1")
	if resp, err := e.client.PostForm(e.srv.URL+"/settings/theme", url.Values{"theme": {"deep"}}); err == nil {
		resp.Body.Close()
	}

	body := e.get(t, "/inbox")
	if !strings.Contains(body, `data-theme="deep"`) {
		t.Fatalf("page should carry the stored theme on <html>:\n%s", body)
	}
}

func TestDefaultThemeFallsBackToOSScript(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "t3@example.com", "password1")

	body := e.get(t, "/inbox")
	// The OS-default logic lives in the (CSP-friendly) external theme.js, loaded
	// before paint; with no stored theme the server must not hardcode data-theme.
	if !strings.Contains(body, "/static/theme.js") {
		t.Fatalf("the theme bootstrap script must be loaded:\n%s", body)
	}
	if strings.Contains(body, "data-theme=") {
		t.Fatalf("with an empty theme, <html> must not hardcode data-theme:\n%s", body)
	}
}

func TestInboxCardShowsDecayVisuals(t *testing.T) {
	e := newTestEnv(t)
	e.register(t, "decay@example.com", "password1")
	e.captureID(t, "https://example.com/x")

	body := e.get(t, "/inbox")
	if !strings.Contains(body, "data-barnacles") {
		t.Fatalf("inbox card should carry data-barnacles:\n%s", body)
	}
	if !strings.Contains(body, "--life:") {
		t.Fatalf("inbox card should carry a --life style:\n%s", body)
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
