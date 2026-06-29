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
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/lesault/tideline/internal/auth"
	"github.com/lesault/tideline/internal/decay"
	"github.com/lesault/tideline/internal/fetch"
	"github.com/lesault/tideline/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

const sessionCookie = "tideline_session"

// Server holds dependencies for the HTTP handlers.
type Server struct {
	store    *store.Store
	sessions *auth.SessionManager
	fetcher  *fetch.Fetcher
	tmpl     map[string]*template.Template
	now      func() time.Time
}

// New constructs a Server. The metadata fetcher may be nil in tests that don't
// exercise enrichment.
func New(st *store.Store, sm *auth.SessionManager, f *fetch.Fetcher) *Server {
	return &Server{
		store:    st,
		sessions: sm,
		fetcher:  f,
		tmpl:     parseTemplates(),
		now:      func() time.Time { return time.Now().UTC() },
	}
}

func parseTemplates() map[string]*template.Template {
	pages := []string{"login", "register", "inbox", "flotsam", "triage", "board"}
	m := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		m[p] = template.Must(template.ParseFS(assets, "templates/base.html", "templates/"+p+".html"))
	}
	return m
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

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
		r.Get("/triage", s.triagePage)
		r.Post("/triage/{id}", s.triageSubmit)
		r.Post("/links/{id}/drop", s.dropLink)
		r.Get("/board", s.boardPage)
		r.Post("/cards/{id}/move", s.moveCard)
		r.Post("/categories", s.addCategory)
	})

	// Authenticated JSON API (401 when signed out).
	r.Group(func(r chi.Router) {
		r.Use(s.apiAuth)
		r.Post("/api/links", s.captureAPI)
		r.Get("/api/links", s.inboxAPI)
	})

	return r
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
		uid, email, ok := s.resolve(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), uid, email)))
	})
}

func withUser(ctx context.Context, uid int64, email string) context.Context {
	ctx = context.WithValue(ctx, ctxUserID, uid)
	return context.WithValue(ctx, ctxEmail, email)
}

func userID(ctx context.Context) int64 { v, _ := ctx.Value(ctxUserID).(int64); return v }
func email(ctx context.Context) string { v, _ := ctx.Value(ctxEmail).(string); return v }

// --- auth handlers ---

func (s *Server) registerSubmit(w http.ResponseWriter, r *http.Request) {
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
	s.startSession(w, u.ID)
	http.Redirect(w, r, "/inbox", http.StatusSeeOther)
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	em := strings.TrimSpace(r.FormValue("email"))
	pw := r.FormValue("password")
	u, err := s.store.UserByEmail(r.Context(), em)
	if err != nil || !auth.VerifyPassword(u.PasswordHash, pw) {
		w.WriteHeader(http.StatusUnauthorized)
		s.renderPage(w, r, "login", map[string]any{"Error": "Invalid email or password."})
		return
	}
	s.startSession(w, u.ID)
	http.Redirect(w, r, "/inbox", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Destroy(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) startSession(w http.ResponseWriter, uid int64) {
	tok := s.sessions.Create(uid)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// --- capture ---

func (s *Server) captureAPI(w http.ResponseWriter, r *http.Request) {
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
	if s.fetcher != nil {
		go s.enrich(link.ID, rawURL, u.Host)
	}
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

func (s *Server) inboxPage(w http.ResponseWriter, r *http.Request) {
	links, err := s.store.ListInbox(r.Context(), userID(r.Context()))
	if err != nil {
		http.Error(w, "could not load inbox", http.StatusInternalServerError)
		return
	}
	s.renderPage(w, r, "inbox", map[string]any{"Links": s.viewLinks(links)})
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

// viewLink is the inbox/flotsam/triage presentation model.
type viewLink struct {
	ID                          int64
	URL, Title, Excerpt, Domain string
	Level                       string
	LevelLabel                  string
	TimeLeft                    string
}

func (s *Server) viewLinks(links []store.Link) []viewLink {
	now := s.now()
	out := make([]viewLink, len(links))
	for i, l := range links {
		lvl := decay.Assess(l.CreatedAt, l.TTLExpiresAt, now)
		out[i] = viewLink{
			ID: l.ID, URL: l.URL, Title: l.Title, Excerpt: l.Excerpt, Domain: l.Domain,
			Level: lvl.String(), LevelLabel: levelLabel(lvl), TimeLeft: timeLeft(l.TTLExpiresAt, now),
		}
	}
	return out
}

// --- triage & board (M2) ---

func (s *Server) triagePage(w http.ResponseWriter, r *http.Request) {
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
	s.renderPage(w, r, "triage", data)
}

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
	err = s.store.TriageLink(r.Context(), userID(r.Context()), id, catID, r.FormValue("next_step"))
	if err == store.ErrNotFound {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not triage", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/triage", http.StatusSeeOther) // straight to the next card
}

func (s *Server) dropLink(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DropLink(r.Context(), userID(r.Context()), id); err != nil && err != store.ErrNotFound {
		http.Error(w, "could not drop", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, backTo(r, "/inbox"), http.StatusSeeOther)
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
		card := boardCard{ID: l.ID, URL: l.URL, Title: l.Title, Domain: l.Domain, NextStep: l.NextStep}
		if l.CategoryID != nil {
			card.Category = catName[*l.CategoryID]
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
	http.Redirect(w, r, backTo(r, "/triage"), http.StatusSeeOther)
}

type boardColumn struct {
	Name  string
	Cards []boardCard
}

type boardCard struct {
	ID                                     int64
	URL, Title, Domain, NextStep, Category string
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
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

// backTo returns the Referer for same-page redirects, falling back to def.
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
