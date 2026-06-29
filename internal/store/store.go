// Package store is Tideline's persistence layer: a thin repository over SQLite
// (pure-Go modernc.org/sqlite, no CGO) with embedded migrations. All access is
// scoped by user id so one account never sees another's links.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Sentinel errors callers can match with errors.Is.
var (
	ErrNotFound  = errors.New("store: not found")
	ErrDuplicate = errors.New("store: duplicate")
)

// Link statuses — the stages of the triage funnel.
const (
	StatusInbox     = "inbox"
	StatusTriaged   = "triaged"
	StatusArchived  = "archived"
	StatusDropped   = "dropped"
	StatusFlotsam   = "flotsam"
	StatusReference = "reference"
)

// Kanban board columns for triaged links, in left-to-right order.
const (
	ColReviewing = "Reviewing"
	ColNext      = "Next"
	ColDone      = "Done"
)

// BoardColumns lists the valid columns in display order.
var BoardColumns = []string{ColReviewing, ColNext, ColDone}

// ValidColumn reports whether c is a known board column.
func ValidColumn(c string) bool {
	for _, col := range BoardColumns {
		if col == c {
			return true
		}
	}
	return false
}

// defaultCategories are seeded for every new account.
var defaultCategories = []string{"Tech", "Read", "Fun"}

// timeFormat is the canonical on-disk time encoding: sortable RFC3339 in UTC.
const timeFormat = time.RFC3339Nano

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// User is an account.
type User struct {
	ID             int64
	Email          string
	PasswordHash   string
	DefaultTTLDays int
	Timezone       string
	CreatedAt      time.Time
}

// Category is a user-defined label for triaged links.
type Category struct {
	ID     int64
	UserID int64
	Name   string
	Color  string
}

// WallabagAccount holds a user's Wallabag credentials (one per user).
type WallabagAccount struct {
	UserID       int64
	BaseURL      string
	ClientID     string
	ClientSecret string
	Username     string
	Password     string
}

// API token scopes.
const (
	ScopeCapture = "capture" // the browser extension: add links + read counts
	ScopeFeed    = "feed"    // the RSS due feed
)

// APIToken is a scoped credential (the raw value is never stored, only its hash).
type APIToken struct {
	ID        int64
	UserID    int64
	Scope     string
	Label     string
	CreatedAt time.Time
}

// Link is a captured URL moving through the funnel.
type Link struct {
	ID              int64
	UserID          int64
	URL             string
	Title           string
	Excerpt         string
	ImageURL        string
	FaviconURL      string
	Domain          string
	Status          string
	Notes           string
	CategoryID      *int64
	NextStep        string
	BoardColumn     string
	BoardPosition   int
	TTLExpiresAt    time.Time
	CreatedAt       time.Time
	ReviewedAt      time.Time
	ArchivedAt      time.Time
	ScheduledFor    time.Time
	WallabagEntryID *int64
	FetchStatus     string
}

// Open opens (creating if needed) the SQLite database at dsn and applies all
// pending migrations.
func Open(dsn string) (*Store, error) {
	// _pragma options keep SQLite honest under concurrent goroutine access.
	db, err := sql.Open("sqlite", dsn+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("ensure migrations table: %w", err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // lexical order == apply order (0001_, 0002_, ...)

	for _, name := range names {
		var applied string
		err := s.db.QueryRow(`SELECT name FROM schema_migrations WHERE name = ?`, name).Scan(&applied)
		if err == nil {
			continue // already applied
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := s.db.Exec(string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := s.db.Exec(`INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			return fmt.Errorf("record migration %s: %w", name, err)
		}
	}
	return nil
}

// --- Users ---

// CreateUser inserts a new account. Returns ErrDuplicate if the email is taken.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (User, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (email, password_hash, created_at) VALUES (?, ?, ?)`,
		email, passwordHash, now.Format(timeFormat))
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrDuplicate
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	id, _ := res.LastInsertId()
	for _, name := range defaultCategories {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO categories (user_id, name) VALUES (?, ?)`, id, name); err != nil {
			return User{}, fmt.Errorf("seed category %q: %w", name, err)
		}
	}
	return User{ID: id, Email: email, PasswordHash: passwordHash, DefaultTTLDays: 14, Timezone: "UTC", CreatedAt: now}, nil
}

// UpdateDefaultTTL sets the number of days new captures live before expiring.
func (s *Store) UpdateDefaultTTL(ctx context.Context, userID int64, days int) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET default_ttl_days = ? WHERE id = ?`, days, userID)
	if err != nil {
		return fmt.Errorf("update default ttl: %w", err)
	}
	return notFoundIfNoRows(res)
}

// ListCategories returns a user's categories, alphabetically.
func (s *Store) ListCategories(ctx context.Context, userID int64) ([]Category, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, user_id, name, color FROM categories WHERE user_id = ? ORDER BY name`, userID)
	if err != nil {
		return nil, fmt.Errorf("query categories: %w", err)
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.UserID, &c.Name, &c.Color); err != nil {
			return nil, fmt.Errorf("scan category: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateCategory adds a category. Returns ErrDuplicate if the user already has
// one with that name.
func (s *Store) CreateCategory(ctx context.Context, userID int64, name string) (Category, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO categories (user_id, name) VALUES (?, ?)`, userID, name)
	if err != nil {
		if isUniqueViolation(err) {
			return Category{}, ErrDuplicate
		}
		return Category{}, fmt.Errorf("insert category: %w", err)
	}
	id, _ := res.LastInsertId()
	return Category{ID: id, UserID: userID, Name: name}, nil
}

// UserByEmail looks up an account by email. Returns ErrNotFound if absent.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	var u User
	var created string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, default_ttl_days, timezone, created_at FROM users WHERE email = ?`, email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DefaultTTLDays, &u.Timezone, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("query user: %w", err)
	}
	u.CreatedAt = parseTime(created)
	return u, nil
}

// UserByID looks up an account by id. Returns ErrNotFound if absent.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	var u User
	var created string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, default_ttl_days, timezone, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DefaultTTLDays, &u.Timezone, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("query user: %w", err)
	}
	u.CreatedAt = parseTime(created)
	return u, nil
}

// ListByStatus returns a user's links in a given status, soonest-expiry first.
func (s *Store) ListByStatus(ctx context.Context, userID int64, status string) ([]Link, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, url, title, excerpt, image_url, favicon_url, domain, status, notes,
			category_id, next_step, board_column, board_position, ttl_expires_at, created_at,
			reviewed_at, archived_at, scheduled_for, wallabag_entry_id, fetch_status
		 FROM links WHERE user_id = ? AND status = ? ORDER BY ttl_expires_at ASC, id ASC`,
		userID, status)
	if err != nil {
		return nil, fmt.Errorf("query by status: %w", err)
	}
	defer rows.Close()
	return scanLinks(rows)
}

// --- Links ---

// CreateLink inserts a captured link. CreatedAt/TTLExpiresAt should be set by
// the caller; Status defaults to inbox and FetchStatus to pending when empty.
func (s *Store) CreateLink(ctx context.Context, l Link) (Link, error) {
	if l.Status == "" {
		l.Status = StatusInbox
	}
	if l.FetchStatus == "" {
		l.FetchStatus = "pending"
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO links (user_id, url, title, excerpt, image_url, favicon_url, domain,
			status, next_step, board_column, board_position, ttl_expires_at, created_at, fetch_status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.UserID, l.URL, l.Title, l.Excerpt, l.ImageURL, l.FaviconURL, l.Domain,
		l.Status, l.NextStep, l.BoardColumn, l.BoardPosition,
		l.TTLExpiresAt.UTC().Format(timeFormat), l.CreatedAt.UTC().Format(timeFormat), l.FetchStatus)
	if err != nil {
		return Link{}, fmt.Errorf("insert link: %w", err)
	}
	l.ID, _ = res.LastInsertId()
	return l, nil
}

// ListInbox returns a user's inbox links, soonest-to-expire first (most urgent).
func (s *Store) ListInbox(ctx context.Context, userID int64) ([]Link, error) {
	return s.ListByStatus(ctx, userID, StatusInbox)
}

// LinkByID fetches a single link. Returns ErrNotFound if absent.
func (s *Store) LinkByID(ctx context.Context, id int64) (Link, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, url, title, excerpt, image_url, favicon_url, domain, status, notes,
			category_id, next_step, board_column, board_position, ttl_expires_at, created_at,
			reviewed_at, archived_at, scheduled_for, wallabag_entry_id, fetch_status
		 FROM links WHERE id = ?`, id)
	if err != nil {
		return Link{}, fmt.Errorf("query link: %w", err)
	}
	defer rows.Close()
	links, err := scanLinks(rows)
	if err != nil {
		return Link{}, err
	}
	if len(links) == 0 {
		return Link{}, ErrNotFound
	}
	return links[0], nil
}

// Metadata is the preview fields filled in asynchronously after capture. It
// mirrors fetch.Metadata but keeps this package free of that dependency.
type Metadata struct {
	Title      string
	Excerpt    string
	ImageURL   string
	FaviconURL string
	Domain     string
}

// UpdateMetadata writes fetched preview fields and the resulting fetch status
// ("ok" or "failed") for a link.
func (s *Store) UpdateMetadata(ctx context.Context, id int64, m Metadata, fetchStatus string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE links SET title = ?, excerpt = ?, image_url = ?, favicon_url = ?, domain = ?, fetch_status = ? WHERE id = ?`,
		m.Title, m.Excerpt, m.ImageURL, m.FaviconURL, m.Domain, fetchStatus, id)
	if err != nil {
		return fmt.Errorf("update metadata: %w", err)
	}
	return nil
}

// SweepExpired moves every inbox link whose TTL has elapsed (<= now) into the
// flotsam in a single statement, returning how many were moved. Idempotent.
func (s *Store) SweepExpired(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE links SET status = ? WHERE status = ? AND ttl_expires_at <= ?`,
		StatusFlotsam, StatusInbox, now.UTC().Format(timeFormat))
	if err != nil {
		return 0, fmt.Errorf("sweep expired: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// TriageInput captures the decisions made when triaging an inbox link onto the
// board.
type TriageInput struct {
	CategoryID   *int64    // nil clears any category
	NextStep     string    // free-text next action
	Column       string    // target board column; empty -> ColReviewing
	ScheduledFor time.Time // zero -> no schedule (stored as "")
}

// TriageLink moves an inbox link onto the board: it records the chosen category
// (nil clears it) and next-step, lands the card in the requested column (default
// Reviewing), optionally schedules it for a future resurfacing, and stamps
// reviewed_at. Scoped to userID — a foreign link yields ErrNotFound. Triaged
// links leave the inbox, so the decay sweep no longer touches them.
func (s *Store) TriageLink(ctx context.Context, userID, linkID int64, in TriageInput) error {
	col := in.Column
	if col == "" {
		col = ColReviewing
	}
	if !ValidColumn(col) {
		return fmt.Errorf("invalid board column %q", col)
	}
	scheduled := ""
	if !in.ScheduledFor.IsZero() {
		scheduled = in.ScheduledFor.UTC().Format(timeFormat)
	}
	pos := s.nextBoardPosition(ctx, userID, col)
	res, err := s.db.ExecContext(ctx,
		`UPDATE links SET status = ?, category_id = ?, next_step = ?, board_column = ?, board_position = ?, reviewed_at = ?, scheduled_for = ?
		 WHERE id = ? AND user_id = ?`,
		StatusTriaged, in.CategoryID, in.NextStep, col, pos, time.Now().UTC().Format(timeFormat), scheduled, linkID, userID)
	if err != nil {
		return fmt.Errorf("triage link: %w", err)
	}
	return notFoundIfNoRows(res)
}

// ScheduledDue returns a user's triaged links whose scheduled_for has arrived
// (non-empty and <= now), soonest-scheduled first. Unscheduled and
// future-scheduled links are excluded.
func (s *Store) ScheduledDue(ctx context.Context, userID int64, now time.Time) ([]Link, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, url, title, excerpt, image_url, favicon_url, domain, status, notes,
			category_id, next_step, board_column, board_position, ttl_expires_at, created_at,
			reviewed_at, archived_at, scheduled_for, wallabag_entry_id, fetch_status
		 FROM links
		 WHERE user_id = ? AND status = ? AND scheduled_for != '' AND scheduled_for <= ?
		 ORDER BY scheduled_for ASC, id ASC`,
		userID, StatusTriaged, now.UTC().Format(timeFormat))
	if err != nil {
		return nil, fmt.Errorf("query scheduled due: %w", err)
	}
	defer rows.Close()
	return scanLinks(rows)
}

// DropLink discards a link (status=dropped). Scoped to userID.
func (s *Store) DropLink(ctx context.Context, userID, linkID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE links SET status = ? WHERE id = ? AND user_id = ?`,
		StatusDropped, linkID, userID)
	if err != nil {
		return fmt.Errorf("drop link: %w", err)
	}
	return notFoundIfNoRows(res)
}

// MoveCard relocates a board card to column at position. Scoped to userID;
// rejects unknown columns.
func (s *Store) MoveCard(ctx context.Context, userID, linkID int64, column string, position int) error {
	if !ValidColumn(column) {
		return fmt.Errorf("invalid board column %q", column)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE links SET board_column = ?, board_position = ? WHERE id = ? AND user_id = ? AND status = ?`,
		column, position, linkID, userID, StatusTriaged)
	if err != nil {
		return fmt.Errorf("move card: %w", err)
	}
	return notFoundIfNoRows(res)
}

// ListBoard returns a user's triaged cards ordered by column then position.
func (s *Store) ListBoard(ctx context.Context, userID int64) ([]Link, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, url, title, excerpt, image_url, favicon_url, domain, status, notes,
			category_id, next_step, board_column, board_position, ttl_expires_at, created_at,
			reviewed_at, archived_at, scheduled_for, wallabag_entry_id, fetch_status
		 FROM links WHERE user_id = ? AND status = ? ORDER BY board_column ASC, board_position ASC, id ASC`,
		userID, StatusTriaged)
	if err != nil {
		return nil, fmt.Errorf("query board: %w", err)
	}
	defer rows.Close()
	return scanLinks(rows)
}

// ReferenceLink promotes a link to the long-lived reference status and stamps
// reviewed_at. Scoped to userID — a foreign link yields ErrNotFound. Reference
// links leave the inbox and the board, so neither the decay sweep nor the board
// touches them.
func (s *Store) ReferenceLink(ctx context.Context, userID, linkID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE links SET status = ?, reviewed_at = ? WHERE id = ? AND user_id = ?`,
		StatusReference, time.Now().UTC().Format(timeFormat), linkID, userID)
	if err != nil {
		return fmt.Errorf("reference link: %w", err)
	}
	return notFoundIfNoRows(res)
}

// UpdateNotes sets the free-text notes on a link. Scoped to userID — a foreign
// link yields ErrNotFound.
func (s *Store) UpdateNotes(ctx context.Context, userID, linkID int64, notes string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE links SET notes = ? WHERE id = ? AND user_id = ?`,
		notes, linkID, userID)
	if err != nil {
		return fmt.Errorf("update notes: %w", err)
	}
	return notFoundIfNoRows(res)
}

// ListReference returns a user's reference-status links, most-recently-reviewed
// first. When query is non-empty it filters case-insensitively across title,
// url, domain, and notes.
func (s *Store) ListReference(ctx context.Context, userID int64, query string) ([]Link, error) {
	sqlText := `SELECT id, user_id, url, title, excerpt, image_url, favicon_url, domain, status, notes,
			category_id, next_step, board_column, board_position, ttl_expires_at, created_at,
			reviewed_at, archived_at, scheduled_for, wallabag_entry_id, fetch_status
		 FROM links WHERE user_id = ? AND status = ?`
	args := []any{userID, StatusReference}
	if query != "" {
		like := "%" + strings.ToLower(query) + "%"
		sqlText += ` AND (lower(title) LIKE ? OR lower(url) LIKE ? OR lower(domain) LIKE ? OR lower(notes) LIKE ?)`
		args = append(args, like, like, like, like)
	}
	sqlText += ` ORDER BY reviewed_at DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("query reference: %w", err)
	}
	defer rows.Close()
	return scanLinks(rows)
}

// --- API tokens ---

// CreateAPIToken mints a new token for userID with the given scope and label.
// It returns the raw token (shown to the user once) and the stored record.
func (s *Store) CreateAPIToken(ctx context.Context, userID int64, scope, label string) (string, APIToken, error) {
	raw := newRawToken()
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO api_tokens (user_id, token_hash, scope, label, created_at) VALUES (?, ?, ?, ?, ?)`,
		userID, hashToken(raw), scope, label, now.Format(timeFormat))
	if err != nil {
		return "", APIToken{}, fmt.Errorf("insert api token: %w", err)
	}
	id, _ := res.LastInsertId()
	return raw, APIToken{ID: id, UserID: userID, Scope: scope, Label: label, CreatedAt: now}, nil
}

// APITokenByValue resolves a raw token to its record. ErrNotFound if unknown.
func (s *Store) APITokenByValue(ctx context.Context, raw string) (APIToken, error) {
	var t APIToken
	var created string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, scope, label, created_at FROM api_tokens WHERE token_hash = ?`, hashToken(raw)).
		Scan(&t.ID, &t.UserID, &t.Scope, &t.Label, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, ErrNotFound
	}
	if err != nil {
		return APIToken{}, fmt.Errorf("query api token: %w", err)
	}
	t.CreatedAt = parseTime(created)
	return t, nil
}

// ListAPITokens returns a user's tokens (metadata only, never the secret).
func (s *Store) ListAPITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, scope, label, created_at FROM api_tokens WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("query api tokens: %w", err)
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		var created string
		if err := rows.Scan(&t.ID, &t.UserID, &t.Scope, &t.Label, &created); err != nil {
			return nil, fmt.Errorf("scan api token: %w", err)
		}
		t.CreatedAt = parseTime(created)
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteAPIToken revokes a token. Scoped to userID.
func (s *Store) DeleteAPIToken(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete api token: %w", err)
	}
	return notFoundIfNoRows(res)
}

func newRawToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// SaveWallabagAccount stores (or replaces) a user's Wallabag credentials.
func (s *Store) SaveWallabagAccount(ctx context.Context, a WallabagAccount) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO wallabag_accounts (user_id, base_url, client_id, client_secret, username, password)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET
		   base_url = excluded.base_url, client_id = excluded.client_id,
		   client_secret = excluded.client_secret, username = excluded.username,
		   password = excluded.password`,
		a.UserID, a.BaseURL, a.ClientID, a.ClientSecret, a.Username, a.Password)
	if err != nil {
		return fmt.Errorf("save wallabag account: %w", err)
	}
	return nil
}

// WallabagAccount loads a user's Wallabag credentials, ErrNotFound if unset.
func (s *Store) WallabagAccount(ctx context.Context, userID int64) (WallabagAccount, error) {
	var a WallabagAccount
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, base_url, client_id, client_secret, username, password FROM wallabag_accounts WHERE user_id = ?`, userID).
		Scan(&a.UserID, &a.BaseURL, &a.ClientID, &a.ClientSecret, &a.Username, &a.Password)
	if errors.Is(err, sql.ErrNoRows) {
		return WallabagAccount{}, ErrNotFound
	}
	if err != nil {
		return WallabagAccount{}, fmt.Errorf("query wallabag account: %w", err)
	}
	return a, nil
}

// ArchiveLink marks a link archived and records its Wallabag entry id. Scoped
// to userID.
func (s *Store) ArchiveLink(ctx context.Context, userID, linkID, entryID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE links SET status = ?, wallabag_entry_id = ?, archived_at = ? WHERE id = ? AND user_id = ?`,
		StatusArchived, entryID, time.Now().UTC().Format(timeFormat), linkID, userID)
	if err != nil {
		return fmt.Errorf("archive link: %w", err)
	}
	return notFoundIfNoRows(res)
}

func (s *Store) nextBoardPosition(ctx context.Context, userID int64, column string) int {
	var p int
	s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(board_position), -1) + 1 FROM links WHERE user_id = ? AND board_column = ?`,
		userID, column).Scan(&p)
	return p
}

// --- helpers ---

func scanLinks(rows *sql.Rows) ([]Link, error) {
	var out []Link
	for rows.Next() {
		var l Link
		var ttl, created, reviewed, archived, scheduled string
		if err := rows.Scan(&l.ID, &l.UserID, &l.URL, &l.Title, &l.Excerpt, &l.ImageURL,
			&l.FaviconURL, &l.Domain, &l.Status, &l.Notes, &l.CategoryID, &l.NextStep, &l.BoardColumn,
			&l.BoardPosition, &ttl, &created, &reviewed, &archived, &scheduled, &l.WallabagEntryID, &l.FetchStatus); err != nil {
			return nil, fmt.Errorf("scan link: %w", err)
		}
		l.TTLExpiresAt = parseTime(ttl)
		l.CreatedAt = parseTime(created)
		l.ReviewedAt = parseTime(reviewed)
		l.ArchivedAt = parseTime(archived)
		l.ScheduledFor = parseTime(scheduled)
		out = append(out, l)
	}
	return out, rows.Err()
}

func notFoundIfNoRows(res sql.Result) error {
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func isUniqueViolation(err error) bool {
	// modernc.org/sqlite surfaces constraint errors in the message text.
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}
