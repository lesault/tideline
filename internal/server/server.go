// Package server wires Tideline's HTTP layer: session-based auth, link capture,
// the urgency-sorted inbox, the flotsam, and the embedded htmx UI.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/lesault/tideline/internal/auth"
	"github.com/lesault/tideline/internal/decay"
	"github.com/lesault/tideline/internal/feed"
	"github.com/lesault/tideline/internal/fetch"
	"github.com/lesault/tideline/internal/store"
	"github.com/lesault/tideline/internal/wallabag"
)

//go:embed templates/*.html static
var assets embed.FS

const sessionCookie = "tideline_session"

// Server holds dependencies for the HTTP handlers.
type Server struct {
	store              *store.Store
	sessions           *auth.SessionManager
	fetcher            *fetch.Fetcher
	wallabag           *wallabag.Client
	tmpl               map[string]*template.Template
	now                func() time.Time
	loginLimiter       *rateLimiter
	registerLimiter    *rateLimiter
	openRegistration   bool
	forceSecureCookies bool
	enrichSem          chan struct{} // bounds concurrent metadata fetches
	dummyHash          string        // verified on unknown-user login to equalize timing
	verifyPassword     func(hash, pw string) bool
}

// maxConcurrentEnrich caps how many capture metadata fetches run at once, so a
// burst of captures can't spawn unbounded goroutines/outbound requests.
const maxConcurrentEnrich = 8

// New constructs a Server. The metadata fetcher and Wallabag client may be nil
// in tests that don't exercise enrichment or archiving.
func New(st *store.Store, sm *auth.SessionManager, f *fetch.Fetcher, wb *wallabag.Client) *Server {
	// A real argon2id hash to verify against on unknown-user logins, so the
	// password check costs the same whether or not the account exists.
	dummy, _ := auth.HashPassword("tideline-login-timing-equalizer")
	return &Server{
		store:            st,
		sessions:         sm,
		fetcher:          f,
		wallabag:         wb,
		tmpl:             parseTemplates(),
		now:              func() time.Time { return time.Now().UTC() },
		loginLimiter:     newRateLimiter(20, time.Minute), // per-IP login attempts
		registerLimiter:  newRateLimiter(10, time.Hour),   // per-IP new accounts
		openRegistration: true,
		enrichSem:        make(chan struct{}, maxConcurrentEnrich),
		dummyHash:        dummy,
		verifyPassword:   auth.VerifyPassword,
	}
}

// scheduleEnrich kicks off metadata enrichment for a captured link if a worker
// slot is free, returning whether it was scheduled. When at capacity it skips
// (the link stays pending) rather than spawning an unbounded goroutine.
func (s *Server) scheduleEnrich(id int64, rawURL, host string) bool {
	if s.fetcher == nil {
		return false
	}
	select {
	case s.enrichSem <- struct{}{}:
		go func() {
			defer func() { <-s.enrichSem }()
			s.enrich(id, rawURL, host)
		}()
		return true
	default:
		return false
	}
}

// SetOpenRegistration enables/disables self-service account creation.
func (s *Server) SetOpenRegistration(v bool) { s.openRegistration = v }

// SetForceSecureCookies forces the Secure flag on the session cookie even when
// the request looks like plain HTTP (e.g. behind a proxy that doesn't set
// X-Forwarded-Proto). Set when the instance is served only over HTTPS.
func (s *Server) SetForceSecureCookies(v bool) { s.forceSecureCookies = v }

func parseTemplates() map[string]*template.Template {
	pages := []string{"login", "register", "inbox", "flotsam", "triage_focus", "board", "reef", "settings", "message"}
	m := make(map[string]*template.Template, len(pages)+1)
	for _, p := range pages {
		m[p] = template.Must(template.ParseFS(assets, "templates/base.html", "templates/fragments.html", "templates/"+p+".html"))
	}
	// "fragments" renders standalone htmx fragments (rows, cards, OOB nodes).
	m["fragments"] = template.Must(template.ParseFS(assets, "templates/fragments.html"))
	return m
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)

	staticFS, _ := fs.Sub(assets, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Public auth routes.
	r.Get("/login", s.page("login"))
	r.Post("/login", s.loginSubmit)
	r.Get("/register", s.page("register"))
	r.Post("/register", s.registerSubmit)
	r.Post("/logout", s.logout)

	// Authenticated HTML routes (redirect to /login when signed out).
	r.Group(func(r chi.Router) {
		r.Use(s.webAuth)
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/inbox", http.StatusSeeOther)
		})
		r.Get("/inbox", s.inboxPage)
		r.Get("/flotsam", s.flotsamPage)
		r.Post("/links", s.captureForm)
		// Inbox and triage are merged; keep /triage working for old links.
		r.Get("/triage", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/inbox", http.StatusMovedPermanently)
		})
		r.Get("/triage/focus", s.triageFocus)
		r.Post("/triage/{id}", s.triageSubmit)
		r.Post("/links/{id}/drop", s.dropLink)
		r.Post("/links/{id}/reference", s.referenceLink)
		r.Post("/links/{id}/notes", s.updateNotes)
		r.Post("/links/{id}/restore", s.restoreLink)
		r.Get("/reef", s.reefPage)
		r.Get("/board", s.boardPage)
		r.Post("/cards/{id}/move", s.moveCard)
		r.Post("/categories", s.addCategory)
		r.Get("/settings", s.settingsPage)
		r.Post("/settings/ttl", s.saveTTL)
		r.Post("/settings/theme", s.saveTheme)
		r.Post("/settings/wallabag", s.saveWallabag)
		r.Post("/links/{id}/archive", s.archiveLink)
		r.Post("/tokens", s.createToken)
		r.Post("/tokens/{id}/delete", s.deleteToken)
	})

	// RSS due feed — authenticated by a feed-scoped token in ?token= (or a
	// browser session), so feed readers can poll it.
	r.Get("/feed/due", s.dueFeed)

	// Authenticated JSON API (session cookie or scoped token).
	r.Group(func(r chi.Router) {
		r.Use(s.apiAuth)
		r.Post("/api/links", s.captureAPI)
		r.Get("/api/links", s.inboxAPI)
		r.Get("/api/count", s.countAPI)
	})

	return r
}

// securityHeaders sets defensive HTTP headers on every response. The CSP keeps
// all scripts first-party (script-src 'self' — the UI loads only /static JS, no
// inline scripts); inline style attributes (e.g. the tide-bar --life) need
// style-src 'unsafe-inline'. frame-ancestors/X-Frame-Options block clickjacking.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; font-src 'self'; connect-src 'self'; "+
				"form-action 'self'; frame-ancestors 'none'; base-uri 'self'")
		next.ServeHTTP(w, r)
	})
}

// RunSweeper periodically moves expired inbox links to the flotsam until the
// context is cancelled. It runs one sweep immediately so a freshly started
// server reflects expiries without waiting a full interval.
func (s *Server) RunSweeper(ctx context.Context, interval time.Duration) {
	sweep := func() {
		if _, err := s.store.SweepExpired(ctx, s.now()); err != nil && ctx.Err() == nil {
			// Best-effort: a failed sweep is retried next tick.
			_ = err
		}
	}
	sweep()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

// --- auth middleware & context ---

type ctxKey int

const (
	ctxUserID ctxKey = iota
	ctxEmail
	ctxScope
)

func (s *Server) resolve(r *http.Request) (int64, string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return 0, "", false
	}
	uid, ok := s.sessions.UserID(c.Value)
	if !ok {
		return 0, "", false
	}
	u, err := s.store.UserByID(r.Context(), uid)
	if err != nil {
		return 0, "", false
	}
	return uid, u.Email, true
}

func (s *Server) webAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, email, ok := s.resolve(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), uid, email)))
	})
}

func (s *Server) apiAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, scope, ok := s.resolveAPI(r, false) // JSON API: header/cookie only
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		ctx := context.WithValue(withUser(r.Context(), uid, ""), ctxScope, scope)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// resolveAPI authenticates by session cookie or a scoped token in the
// Authorization: Bearer header. The ?token= query form is only honoured when
// allowQueryToken is set (the RSS feed, where readers can't send headers) —
// keeping tokens out of URLs/logs for the JSON API.
func (s *Server) resolveAPI(r *http.Request, allowQueryToken bool) (int64, string, bool) {
	if uid, _, ok := s.resolve(r); ok {
		return uid, scopeSession, true
	}
	raw := bearerToken(r)
	if raw == "" && allowQueryToken {
		raw = r.URL.Query().Get("token")
	}
	if raw != "" {
		if t, err := s.store.APITokenByValue(r.Context(), raw); err == nil {
			return t.UserID, t.Scope, true
		}
	}
	return 0, "", false
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func withUser(ctx context.Context, uid int64, email string) context.Context {
	ctx = context.WithValue(ctx, ctxUserID, uid)
	return context.WithValue(ctx, ctxEmail, email)
}

const scopeSession = "session"

func userID(ctx context.Context) int64 { v, _ := ctx.Value(ctxUserID).(int64); return v }
func email(ctx context.Context) string { v, _ := ctx.Value(ctxEmail).(string); return v }
func scope(ctx context.Context) string { v, _ := ctx.Value(ctxScope).(string); return v }

// --- auth handlers ---

func (s *Server) registerSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.openRegistration {
		http.Error(w, "registration is closed", http.StatusForbidden)
		return
	}
	if !s.registerLimiter.allow(clientIP(r)) {
		http.Error(w, "too many attempts — try again later", http.StatusTooManyRequests)
		return
	}
	em := strings.TrimSpace(r.FormValue("email"))
	pw := r.FormValue("password")
	if em == "" || len(pw) < 8 {
		s.renderPage(w, r, "register", map[string]any{"Error": "Enter an email and a password of at least 8 characters."})
		return
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	u, err := s.store.CreateUser(r.Context(), em, hash)
	if err != nil {
		s.renderPage(w, r, "register", map[string]any{"Error": "That email is already registered."})
		return
	}
	s.startSession(w, r, u.ID)
	http.Redirect(w, r, "/inbox", http.StatusSeeOther)
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.loginLimiter.allow(clientIP(r)) {
		http.Error(w, "too many attempts — try again later", http.StatusTooManyRequests)
		return
	}
	em := strings.TrimSpace(r.FormValue("email"))
	pw := r.FormValue("password")
	u, err := s.store.UserByEmail(r.Context(), em)
	// Always run the (slow) password check — against a dummy hash when the user
	// doesn't exist — so response time doesn't reveal whether the email is valid.
	hash := s.dummyHash
	if err == nil {
		hash = u.PasswordHash
	}
	valid := s.verifyPassword(hash, pw)
	if err != nil || !valid {
		w.WriteHeader(http.StatusUnauthorized)
		s.renderPage(w, r, "login", map[string]any{"Error": "Invalid email or password."})
		return
	}
	s.startSession(w, r, u.ID)
	http.Redirect(w, r, "/inbox", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Destroy(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, uid int64) {
	tok := s.sessions.Create(uid)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookie(r),
	})
}

// secureCookie reports whether the session cookie should carry the Secure flag:
// when the request arrived over TLS (directly or via a TLS-terminating proxy
// setting X-Forwarded-Proto), or when forced on by config.
func (s *Server) secureCookie(r *http.Request) bool {
	return s.forceSecureCookies || r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// --- capture ---

func (s *Server) captureAPI(w http.ResponseWriter, r *http.Request) {
	if sc := scope(r.Context()); sc != scopeSession && sc != store.ScopeCapture {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "token lacks capture scope"})
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	link, err := s.capture(r.Context(), userID(r.Context()), body.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, toAPILink(link))
}

func (s *Server) captureForm(w http.ResponseWriter, r *http.Request) {
	if _, err := s.capture(r.Context(), userID(r.Context()), r.FormValue("url")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/inbox", http.StatusSeeOther)
}

// capture validates and stores a link, then kicks off async metadata enrichment.
func (s *Server) capture(ctx context.Context, uid int64, rawURL string) (store.Link, error) {
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return store.Link{}, fmt.Errorf("not a valid http(s) URL")
	}
	user, err := s.store.UserByID(ctx, uid)
	if err != nil {
		return store.Link{}, err
	}
	now := s.now()
	link, err := s.store.CreateLink(ctx, store.Link{
		UserID:       uid,
		URL:          rawURL,
		Domain:       u.Host,
		CreatedAt:    now,
		TTLExpiresAt: now.AddDate(0, 0, user.DefaultTTLDays),
	})
	if err != nil {
		return store.Link{}, err
	}
	s.scheduleEnrich(link.ID, rawURL, u.Host)
	return link, nil
}

// enrich fetches metadata for a freshly captured link and records the outcome.
// Failure never loses the link — it just marks fetch_status failed.
func (s *Server) enrich(id int64, rawURL, host string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	md, err := s.fetcher.Fetch(ctx, rawURL)
	if err != nil {
		s.store.UpdateMetadata(ctx, id, store.Metadata{Domain: host}, "failed")
		return
	}
	s.store.UpdateMetadata(ctx, id, store.Metadata{
		Title: md.Title, Excerpt: md.Excerpt, ImageURL: md.ImageURL,
		FaviconURL: md.FaviconURL, Domain: md.Domain,
	}, "ok")
}

// --- views ---

// inboxPage is the merged inbox+triage: capture, plus the urgency-sorted list of
// inbox links with inline triage controls (Focus mode offers the stepped view).
func (s *Server) inboxPage(w http.ResponseWriter, r *http.Request) {
	uid := userID(r.Context())
	links, err := s.store.ListInbox(r.Context(), uid)
	if err != nil {
		http.Error(w, "could not load inbox", http.StatusInternalServerError)
		return
	}
	cats, _ := s.store.ListCategories(r.Context(), uid)
	vls := s.viewLinks(links)
	rows := make([]triageRowVM, len(vls))
	for i, vl := range vls {
		rows[i] = triageRowVM{viewLink: vl, Categories: cats, Return: "/inbox"}
	}
	s.renderPage(w, r, "inbox", map[string]any{
		"Categories": cats,
		"Remaining":  len(links),
		"Rows":       rows,
	})
}

func (s *Server) flotsamPage(w http.ResponseWriter, r *http.Request) {
	links, err := s.store.ListByStatus(r.Context(), userID(r.Context()), store.StatusFlotsam)
	if err != nil {
		http.Error(w, "could not load flotsam", http.StatusInternalServerError)
		return
	}
	s.renderPage(w, r, "flotsam", map[string]any{"Links": s.viewLinks(links)})
}

func (s *Server) inboxAPI(w http.ResponseWriter, r *http.Request) {
	links, err := s.store.ListInbox(r.Context(), userID(r.Context()))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not load inbox"})
		return
	}
	out := make([]apiLink, len(links))
	for i, l := range links {
		out[i] = toAPILink(l)
	}
	writeJSON(w, http.StatusOK, out)
}

// viewLink is the inbox/flotsam/triage presentation model. It carries the
// "decay made visible" fields: an urgency class, the draining tide-bar width
// (as a ready-to-use --life custom property), the barnacle count, and a stable
// per-link seed so each card grows a unique-but-deterministic crust.
type viewLink struct {
	ID                          int64
	URL, Title, Excerpt, Domain string
	Level                       string
	LevelLabel                  string
	TimeLeft                    string
	Urgency                     string       // u-fresh | u-aging | u-due | u-expired
	Style                       template.CSS // inline "--life:0.NN" for the tide bar
	Barnacles                   int          // how many crust dots to scatter
	Seed                        int64        // PRNG seed (the link ID)
}

func (s *Server) viewLinks(links []store.Link) []viewLink {
	now := s.now()
	out := make([]viewLink, len(links))
	for i, l := range links {
		lvl := decay.Assess(l.CreatedAt, l.TTLExpiresAt, now)
		life := decay.LifeRemaining(l.CreatedAt, l.TTLExpiresAt, now)
		out[i] = viewLink{
			ID: l.ID, URL: l.URL, Title: l.Title, Excerpt: l.Excerpt, Domain: l.Domain,
			Level: lvl.String(), LevelLabel: levelLabel(lvl), TimeLeft: timeLeft(l.TTLExpiresAt, now),
			Urgency:   urgencyClass(lvl),
			Style:     template.CSS(fmt.Sprintf("--life:%.2f", life)),
			Barnacles: decay.BarnacleCount(lvl),
			Seed:      l.ID,
		}
	}
	return out
}

// urgencyClass maps a decay Level to the CSS class the design uses on cards.
func urgencyClass(l decay.Level) string {
	switch l {
	case decay.Fresh:
		return "u-fresh"
	case decay.Aging:
		return "u-aging"
	case decay.DueSoon:
		return "u-due"
	default:
		return "u-expired"
	}
}

// --- triage & board (M2) ---

// triageFocus renders the focus view: just the single most-urgent inbox link.
func (s *Server) triageFocus(w http.ResponseWriter, r *http.Request) {
	uid := userID(r.Context())
	links, err := s.store.ListInbox(r.Context(), uid)
	if err != nil {
		http.Error(w, "could not load inbox", http.StatusInternalServerError)
		return
	}
	cats, _ := s.store.ListCategories(r.Context(), uid)
	data := map[string]any{"Categories": cats, "Remaining": len(links)}
	if len(links) > 0 {
		data["Card"] = s.viewLinks(links[:1])[0] // most urgent
	}
	s.renderPage(w, r, "triage_focus", data)
}

const scheduleFormat = "2006-01-02"

func (s *Server) triageSubmit(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var catID *int64
	if v := strings.TrimSpace(r.FormValue("category_id")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			catID = &n
		}
	}
	in := store.TriageInput{CategoryID: catID}
	switch r.FormValue("next_step") {
	case "schedule":
		when := s.now().AddDate(0, 0, 3)
		if v := strings.TrimSpace(r.FormValue("scheduled_for")); v != "" {
			d, err := time.Parse(scheduleFormat, v)
			if err != nil {
				http.Error(w, "invalid scheduled_for date", http.StatusBadRequest)
				return
			}
			when = d
		}
		in.NextStep = "schedule"
		in.Column = store.ColReviewing
		in.ScheduledFor = when
	case "review":
		in.NextStep = "review"
		in.Column = store.ColReviewing
	default:
		http.Error(w, "unknown next step", http.StatusBadRequest)
		return
	}
	err = s.store.TriageLink(r.Context(), userID(r.Context()), id, in)
	if err == store.ErrNotFound {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not triage", http.StatusInternalServerError)
		return
	}
	if isHTMX(r) {
		msg := "Scheduled"
		if in.NextStep == "review" {
			msg = "Sent to review"
		}
		// Triage only acts on inbox links, so the prior state is always inbox.
		s.htmxRemovalResponse(w, r, store.Link{ID: id, Status: store.StatusInbox}, msg, true)
		return
	}
	// Return the user to whichever view they came from.
	dest := "/inbox"
	if ret := r.FormValue("return"); ret == "/inbox" || ret == "/triage/focus" {
		dest = ret
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) dropLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	uid := userID(r.Context())
	var prior store.Link
	if isHTMX(r) {
		// Capture the prior state so the undo toast can restore it.
		prior, _ = s.store.LinkByID(r.Context(), id)
	}
	if err := s.store.DropLink(r.Context(), uid, id); err != nil && err != store.ErrNotFound {
		http.Error(w, "could not drop", http.StatusInternalServerError)
		return
	}
	if isHTMX(r) {
		s.htmxRemovalResponse(w, r, prior, "Dropped", true)
		return
	}
	http.Redirect(w, r, backTo(r, "/inbox"), http.StatusSeeOther)
}

// referenceLink promotes a board card to the long-lived reference library.
func (s *Server) referenceLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	uid := userID(r.Context())
	var prior store.Link
	if isHTMX(r) {
		prior, _ = s.store.LinkByID(r.Context(), id)
	}
	// Merged note + verdict: persist any note typed on the card before promoting.
	s.persistNotesIfPresent(r, uid, id)
	if err := s.store.ReferenceLink(r.Context(), uid, id); err != nil {
		if err == store.ErrNotFound {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not reference", http.StatusInternalServerError)
		return
	}
	if isHTMX(r) {
		s.htmxRemovalResponse(w, r, prior, "Referenced", true)
		return
	}
	http.Redirect(w, r, backTo(r, "/board"), http.StatusSeeOther)
}

// updateNotes saves the free-text note on a link.
func (s *Server) updateNotes(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	uid := userID(r.Context())
	if err := s.store.UpdateNotes(r.Context(), uid, id, r.FormValue("notes")); err != nil {
		if err == store.ErrNotFound {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not save notes", http.StatusInternalServerError)
		return
	}
	if isHTMX(r) {
		// Return the refreshed card so its note input keeps the saved value.
		link, err := s.store.LinkByID(r.Context(), id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		s.frag(w, "board_card", s.cardModel(r.Context(), uid, link))
		return
	}
	http.Redirect(w, r, backTo(r, "/board"), http.StatusSeeOther)
}

// reefPage shows the Reef — the searchable reference collection, optionally
// filtered by ?q=.
func (s *Server) reefPage(w http.ResponseWriter, r *http.Request) {
	uid := userID(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	links, err := s.store.ListReference(r.Context(), uid, query)
	if err != nil {
		http.Error(w, "could not load the reef", http.StatusInternalServerError)
		return
	}
	cats, _ := s.store.ListCategories(r.Context(), uid)
	catName := map[int64]string{}
	for _, c := range cats {
		catName[c.ID] = c.Name
	}
	items := make([]libraryItem, len(links))
	for i, l := range links {
		it := libraryItem{ID: l.ID, URL: l.URL, Title: l.Title, Domain: l.Domain, Notes: l.Notes}
		if l.CategoryID != nil {
			it.Category = catName[*l.CategoryID]
		}
		items[i] = it
	}
	s.renderPage(w, r, "reef", map[string]any{"Items": items, "Query": query})
}

func (s *Server) boardPage(w http.ResponseWriter, r *http.Request) {
	uid := userID(r.Context())
	cards, err := s.store.ListBoard(r.Context(), uid)
	if err != nil {
		http.Error(w, "could not load board", http.StatusInternalServerError)
		return
	}
	cats, _ := s.store.ListCategories(r.Context(), uid)
	catName := map[int64]string{}
	for _, c := range cats {
		catName[c.ID] = c.Name
	}
	columns := make([]boardColumn, len(store.BoardColumns))
	index := map[string]int{}
	for i, name := range store.BoardColumns {
		columns[i] = boardColumn{Name: name}
		index[name] = i
	}
	for _, l := range cards {
		card := boardCard{ID: l.ID, URL: l.URL, Title: l.Title, Domain: l.Domain, NextStep: l.NextStep, Notes: l.Notes}
		if l.CategoryID != nil {
			card.Category = catName[*l.CategoryID]
		}
		if !l.ScheduledFor.IsZero() {
			card.Scheduled = l.ScheduledFor.Format(scheduleFormat)
			card.Overdue = !l.ScheduledFor.After(s.now())
		}
		if i, ok := index[l.BoardColumn]; ok {
			columns[i].Cards = append(columns[i].Cards, card)
		} else {
			columns[index[store.ColReviewing]].Cards = append(columns[index[store.ColReviewing]].Cards, card)
		}
	}
	s.renderPage(w, r, "board", map[string]any{"Columns": columns})
}

func (s *Server) moveCard(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	column := r.FormValue("column")
	if !store.ValidColumn(column) {
		http.Error(w, "invalid column", http.StatusBadRequest)
		return
	}
	pos, _ := strconv.Atoi(r.FormValue("position"))
	if err := s.store.MoveCard(r.Context(), userID(r.Context()), id, column, pos); err != nil {
		if err == store.ErrNotFound {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "could not move", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) addCategory(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name != "" {
		// A duplicate is harmless here — the user just re-added an existing label.
		if _, err := s.store.CreateCategory(r.Context(), userID(r.Context()), name); err != nil && err != store.ErrDuplicate {
			http.Error(w, "could not add category", http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, backTo(r, "/inbox"), http.StatusSeeOther)
}

// --- nudges: due feed, count, tokens (M4) ---

// dueLinks returns the inbox links that have reached DueSoon or beyond — the
// ones the nudges (badge, feed) should surface.
func (s *Server) dueLinks(ctx context.Context, uid int64) ([]store.Link, error) {
	links, err := s.store.ListInbox(ctx, uid)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var due []store.Link
	for _, l := range links {
		if decay.Assess(l.CreatedAt, l.TTLExpiresAt, now) >= decay.DueSoon {
			due = append(due, l)
		}
	}
	// Scheduled triaged links whose date has arrived resurface alongside the
	// decaying inbox. The two sets are disjoint (inbox vs triaged).
	scheduled, err := s.store.ScheduledDue(ctx, uid, now)
	if err != nil {
		return nil, err
	}
	due = append(due, scheduled...)
	return due, nil
}

func (s *Server) countAPI(w http.ResponseWriter, r *http.Request) {
	due, err := s.dueLinks(r.Context(), userID(r.Context()))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not count"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"count": len(due)})
}

func (s *Server) dueFeed(w http.ResponseWriter, r *http.Request) {
	uid, sc, ok := s.resolveAPI(r, true) // RSS feed: ?token= allowed
	if !ok || (sc != scopeSession && sc != store.ScopeFeed) {
		http.Error(w, "a feed-scoped token is required", http.StatusUnauthorized)
		return
	}
	due, err := s.dueLinks(r.Context(), uid)
	if err != nil {
		http.Error(w, "could not build feed", http.StatusInternalServerError)
		return
	}
	now := s.now()
	base := externalBase(r)
	items := make([]feed.Item, len(due))
	for i, l := range due {
		title := l.Title
		if title == "" {
			title = l.URL
		}
		items[i] = feed.Item{
			Title:       title,
			URL:         l.URL,
			Description: fmt.Sprintf("%s — %s", l.Domain, timeLeft(l.TTLExpiresAt, now)),
			GUID:        fmt.Sprintf("tideline-%d", l.ID),
			Published:   l.CreatedAt,
		}
	}
	xml, err := feed.Render("Tideline — due links", base+"/inbox", "Links nearing expiry in your Tideline inbox", items)
	if err != nil {
		http.Error(w, "could not render feed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Write([]byte(xml))
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	sc := r.FormValue("scope")
	if sc != store.ScopeCapture && sc != store.ScopeFeed {
		http.Error(w, "invalid scope", http.StatusBadRequest)
		return
	}
	raw, _, err := s.store.CreateAPIToken(r.Context(), userID(r.Context()), sc, strings.TrimSpace(r.FormValue("label")))
	if err != nil {
		http.Error(w, "could not create token", http.StatusInternalServerError)
		return
	}
	// Show the raw token exactly once, inline on the settings page.
	s.renderSettings(w, r, raw)
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteAPIToken(r.Context(), userID(r.Context()), id); err != nil && err != store.ErrNotFound {
		http.Error(w, "could not delete token", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// --- settings & Wallabag archiving (M3) ---

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, "")
}

// maxTTLDays caps the configurable TTL at a sane ~10 years.
const maxTTLDays = 3650

func (s *Server) saveTTL(w http.ResponseWriter, r *http.Request) {
	days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("days")))
	if err != nil || days < 1 || days > maxTTLDays {
		http.Error(w, "TTL must be a whole number of days between 1 and 3650", http.StatusBadRequest)
		return
	}
	if err := s.store.UpdateDefaultTTL(r.Context(), userID(r.Context()), days); err != nil {
		http.Error(w, "could not save TTL", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// renderSettings draws the settings page; newToken, when non-empty, is shown
// once as a freshly minted API token.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, newToken string) {
	uid := userID(r.Context())
	data := map[string]any{"DefaultTTLDays": 14}
	if u, err := s.store.UserByID(r.Context(), uid); err == nil {
		data["DefaultTTLDays"] = u.DefaultTTLDays
	}
	if acct, err := s.store.WallabagAccount(r.Context(), uid); err == nil {
		// Echo everything except the password, which we never render back.
		data["Wallabag"] = map[string]string{
			"BaseURL": acct.BaseURL, "ClientID": acct.ClientID,
			"ClientSecret": acct.ClientSecret, "Username": acct.Username,
		}
	}
	if tokens, err := s.store.ListAPITokens(r.Context(), uid); err == nil {
		data["Tokens"] = tokens
	}
	if newToken != "" {
		data["NewToken"] = newToken
		data["FeedURL"] = externalBase(r) + "/feed/due?token=" + newToken
	}
	s.renderPage(w, r, "settings", data)
}

// saveTheme persists the user's chosen theme. "" means follow the OS; the
// other three are the named tidal palettes. Anything else is a bad request.
func (s *Server) saveTheme(w http.ResponseWriter, r *http.Request) {
	theme := r.FormValue("theme")
	switch theme {
	case "", "deep", "foam", "table":
	default:
		http.Error(w, "invalid theme", http.StatusBadRequest)
		return
	}
	if err := s.store.UpdateTheme(r.Context(), userID(r.Context()), theme); err != nil {
		http.Error(w, "could not save theme", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Server) saveWallabag(w http.ResponseWriter, r *http.Request) {
	acct := store.WallabagAccount{
		UserID:       userID(r.Context()),
		BaseURL:      strings.TrimSpace(r.FormValue("base_url")),
		ClientID:     strings.TrimSpace(r.FormValue("client_id")),
		ClientSecret: strings.TrimSpace(r.FormValue("client_secret")),
		Username:     strings.TrimSpace(r.FormValue("username")),
		Password:     r.FormValue("password"),
	}
	if err := s.store.SaveWallabagAccount(r.Context(), acct); err != nil {
		http.Error(w, "could not save settings", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// archiveLink pushes a link to the user's Wallabag, then marks it archived. A
// failure leaves the link untouched and shows a retryable message — never lost.
func (s *Server) archiveLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	uid := userID(r.Context())
	link, err := s.store.LinkByID(r.Context(), id)
	if err != nil || link.UserID != uid {
		http.NotFound(w, r)
		return
	}
	// Merged note + verdict: persist any note typed on the card before sending.
	s.persistNotesIfPresent(r, uid, id)
	acct, err := s.store.WallabagAccount(r.Context(), uid)
	if err == store.ErrNotFound {
		if isHTMX(r) {
			w.Header().Set("HX-Redirect", "/settings")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if err != nil {
		http.Error(w, "could not load Wallabag settings", http.StatusInternalServerError)
		return
	}

	entryID, err := s.wallabag.Archive(r.Context(), wallabag.Config{
		BaseURL: acct.BaseURL, ClientID: acct.ClientID, ClientSecret: acct.ClientSecret,
		Username: acct.Username, Password: acct.Password,
	}, link.URL)
	if err != nil {
		if isHTMX(r) {
			// Keep the card/row in place; surface a non-undoable error toast.
			w.Header().Set("HX-Reswap", "none")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			s.frag(w, "toast_oob", toastVM{Message: "Wallabag push failed — your link is safe."})
			return
		}
		s.renderPage(w, r, "message", map[string]any{
			"Title": "Wallabag push failed",
			"Body":  "Couldn't send this link to Wallabag. Your link is safe and still here. Check your Wallabag settings and try again.",
			"Back":  backTo(r, "/board"),
		})
		return
	}
	if err := s.store.ArchiveLink(r.Context(), uid, id, entryID); err != nil {
		http.Error(w, "archived in Wallabag but could not update link", http.StatusInternalServerError)
		return
	}
	if isHTMX(r) {
		// Sending to Wallabag can't be un-sent, so the toast offers no Undo.
		s.htmxRemovalResponse(w, r, link, "Sent to Wallabag", false)
		return
	}
	http.Redirect(w, r, backTo(r, "/board"), http.StatusSeeOther)
}

type boardColumn struct {
	Name  string
	Cards []boardCard
}

type boardCard struct {
	ID                                            int64
	URL, Title, Domain, NextStep, Category, Notes string
	Scheduled                                     string // formatted scheduled date, "" if none
	Overdue                                       bool   // scheduled date has arrived/passed
}

// libraryItem is the reference-library presentation model.
type libraryItem struct {
	ID                                  int64
	URL, Title, Domain, Category, Notes string
}

// --- htmx fragments, undo toast & restore (M5) ---

// isHTMX reports whether the request came from htmx (vs. a plain form submit).
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// triageRowVM carries a triage row plus the data its block needs.
type triageRowVM struct {
	viewLink
	Categories []store.Category
	Return     string
}

// toastVM drives the OOB undo toast. A non-empty Status renders an Undo form.
type toastVM struct {
	Message string
	LinkID  int64
	Status  string // prior status to restore to: "inbox" or "triaged"; "" = no undo
	Column  string // prior board column (for triaged restores)
}

// colCountVM drives an OOB board-column count update.
type colCountVM struct {
	Name  string
	Count int
}

// frag renders a single named fragment template to w (best effort).
func (s *Server) frag(w http.ResponseWriter, name string, data any) {
	_ = s.tmpl["fragments"].ExecuteTemplate(w, name, data)
}

// persistNotesIfPresent saves the optional "notes" field so a typed note plus a
// verdict click is a single action. Absent field => no change.
func (s *Server) persistNotesIfPresent(r *http.Request, uid, id int64) {
	if err := r.ParseForm(); err != nil {
		return
	}
	if _, ok := r.Form["notes"]; ok {
		_ = s.store.UpdateNotes(r.Context(), uid, id, r.Form.Get("notes"))
	}
}

func (s *Server) inboxCount(ctx context.Context, uid int64) int {
	links, _ := s.store.ListInbox(ctx, uid)
	return len(links)
}

func (s *Server) boardColCount(ctx context.Context, uid int64, col string) int {
	cards, _ := s.store.ListBoard(ctx, uid)
	n := 0
	for _, c := range cards {
		bc := c.BoardColumn
		if bc == "" {
			bc = store.ColReviewing
		}
		if bc == col {
			n++
		}
	}
	return n
}

// cardModel builds the board-card presentation model for a single link.
func (s *Server) cardModel(ctx context.Context, uid int64, l store.Link) boardCard {
	cats, _ := s.store.ListCategories(ctx, uid)
	name := map[int64]string{}
	for _, c := range cats {
		name[c.ID] = c.Name
	}
	card := boardCard{ID: l.ID, URL: l.URL, Title: l.Title, Domain: l.Domain, NextStep: l.NextStep, Notes: l.Notes}
	if l.CategoryID != nil {
		card.Category = name[*l.CategoryID]
	}
	if !l.ScheduledFor.IsZero() {
		card.Scheduled = l.ScheduledFor.Format(scheduleFormat)
		card.Overdue = !l.ScheduledFor.After(s.now())
	}
	return card
}

// htmxRemovalResponse writes the 200 fragment for an action that removed a
// row/card: the empty target (caller's hx-target swaps to nothing), an OOB count
// for the affected view, and an OOB toast (with Undo when undoable). prior is the
// link's state *before* the action, used to pick the count target and undo data.
func (s *Server) htmxRemovalResponse(w http.ResponseWriter, r *http.Request, prior store.Link, msg string, undoable bool) {
	ctx := r.Context()
	uid := userID(ctx)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	toast := toastVM{Message: msg, LinkID: prior.ID}
	if prior.Status == store.StatusTriaged {
		col := prior.BoardColumn
		if col == "" {
			col = store.ColReviewing
		}
		s.frag(w, "board_count_oob", colCountVM{Name: col, Count: s.boardColCount(ctx, uid, col)})
		if undoable {
			toast.Status = store.StatusTriaged
			toast.Column = col
		}
	} else {
		s.frag(w, "inbox_count_oob", s.inboxCount(ctx, uid))
		if undoable {
			toast.Status = store.StatusInbox
		}
	}
	s.frag(w, "toast_oob", toast)
}

// restoreLink undoes a triage/drop/reference (or the explicit Flotsam/Library
// restore buttons) by returning a link to the inbox or the board.
func (s *Server) restoreLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	uid := userID(r.Context())
	switch r.FormValue("status") {
	case store.StatusInbox:
		days := 14
		if u, err := s.store.UserByID(r.Context(), uid); err == nil {
			days = u.DefaultTTLDays
		}
		err = s.store.RestoreToInbox(r.Context(), uid, id, s.now().AddDate(0, 0, days))
		if err == store.ErrNotFound {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "could not restore", http.StatusInternalServerError)
			return
		}
		s.redirectOrHX(w, r, "/inbox")
	case store.StatusTriaged:
		col := r.FormValue("column")
		if col == "" {
			col = store.ColReviewing
		}
		err = s.store.RestoreToBoard(r.Context(), uid, id, col)
		if err == store.ErrNotFound {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "could not restore", http.StatusInternalServerError)
			return
		}
		s.redirectOrHX(w, r, "/board")
	default:
		http.Error(w, "unknown restore status", http.StatusBadRequest)
	}
}

// redirectOrHX sends an htmx client to dest via HX-Redirect, otherwise a 303.
func (s *Server) redirectOrHX(w http.ResponseWriter, r *http.Request, dest string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func levelLabel(l decay.Level) string {
	switch l {
	case decay.Fresh:
		return "Fresh"
	case decay.Aging:
		return "Aging"
	case decay.DueSoon:
		return "Due soon"
	default:
		return "Expired"
	}
}

func timeLeft(expires, now time.Time) string {
	d := expires.Sub(now)
	if d <= 0 {
		return "expired"
	}
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd left", int(d.Hours())/24)
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh left", int(d.Hours()))
	}
	return fmt.Sprintf("%dm left", int(d.Minutes()))
}

// --- rendering & helpers ---

func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// If already logged in, send auth pages to the inbox.
		if _, _, ok := s.resolve(r); ok && (name == "login" || name == "register") {
			http.Redirect(w, r, "/inbox", http.StatusSeeOther)
			return
		}
		if name == "register" && !s.openRegistration {
			s.renderPage(w, r, "message", map[string]any{
				"Title": "Registration closed",
				"Body":  "This Tideline instance isn't accepting new accounts. Ask the owner for an invite.",
				"Back":  "/login",
			})
			return
		}
		s.renderPage(w, r, name, map[string]any{})
	}
}

func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	t, ok := s.tmpl[name]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	if _, hasEmail := data["UserEmail"]; !hasEmail {
		data["UserEmail"] = email(r.Context())
	}
	if _, hasTheme := data["Theme"]; !hasTheme {
		data["Theme"] = s.userTheme(r.Context())
	}
	if _, hasReg := data["OpenRegistration"]; !hasReg {
		data["OpenRegistration"] = s.openRegistration
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// userTheme returns the signed-in user's stored theme ("" = follow the OS,
// resolved client-side before paint). Unauthenticated pages get "".
func (s *Server) userTheme(ctx context.Context) string {
	uid := userID(ctx)
	if uid == 0 {
		return ""
	}
	u, err := s.store.UserByID(ctx, uid)
	if err != nil {
		return ""
	}
	return u.Theme
}

type apiLink struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	Title       string `json:"title"`
	Excerpt     string `json:"excerpt"`
	Domain      string `json:"domain"`
	Status      string `json:"status"`
	FetchStatus string `json:"fetch_status"`
	ExpiresAt   string `json:"expires_at"`
}

func toAPILink(l store.Link) apiLink {
	return apiLink{
		ID: l.ID, URL: l.URL, Title: l.Title, Excerpt: l.Excerpt, Domain: l.Domain,
		Status: l.Status, FetchStatus: l.FetchStatus, ExpiresAt: l.TTLExpiresAt.Format(time.RFC3339),
	}
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

// externalBase reconstructs the externally-visible base URL (scheme://host),
// honouring a reverse proxy's X-Forwarded-Proto.
func externalBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + r.Host
}

// backTo returns the Referer for same-page redirects, falling back to def.
// clientIP is the connecting peer's address, used as the rate-limit key. Behind
// a reverse proxy this is the proxy; trusted-header parsing can be added later.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func backTo(r *http.Request, def string) string {
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Path != "" {
			return u.Path
		}
	}
	return def
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
