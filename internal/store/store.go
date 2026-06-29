// Package store is Tideline's persistence layer: a thin repository over SQLite
// (pure-Go modernc.org/sqlite, no CGO) with embedded migrations. All access is
// scoped by user id so one account never sees another's links.
package store

import (
	"context"
	"database/sql"
	"embed"
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
	StatusGraveyard = "graveyard"
)

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
	CategoryID      *int64
	NextStep        string
	BoardColumn     string
	BoardPosition   int
	TTLExpiresAt    time.Time
	CreatedAt       time.Time
	ReviewedAt      time.Time
	ArchivedAt      time.Time
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
	return User{ID: id, Email: email, PasswordHash: passwordHash, DefaultTTLDays: 14, Timezone: "UTC", CreatedAt: now}, nil
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
		`SELECT id, user_id, url, title, excerpt, image_url, favicon_url, domain, status,
			category_id, next_step, board_column, board_position, ttl_expires_at, created_at,
			reviewed_at, archived_at, wallabag_entry_id, fetch_status
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
		`SELECT id, user_id, url, title, excerpt, image_url, favicon_url, domain, status,
			category_id, next_step, board_column, board_position, ttl_expires_at, created_at,
			reviewed_at, archived_at, wallabag_entry_id, fetch_status
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
// graveyard in a single statement, returning how many were moved. Idempotent.
func (s *Store) SweepExpired(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE links SET status = ? WHERE status = ? AND ttl_expires_at <= ?`,
		StatusGraveyard, StatusInbox, now.UTC().Format(timeFormat))
	if err != nil {
		return 0, fmt.Errorf("sweep expired: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// --- helpers ---

func scanLinks(rows *sql.Rows) ([]Link, error) {
	var out []Link
	for rows.Next() {
		var l Link
		var ttl, created, reviewed, archived string
		if err := rows.Scan(&l.ID, &l.UserID, &l.URL, &l.Title, &l.Excerpt, &l.ImageURL,
			&l.FaviconURL, &l.Domain, &l.Status, &l.CategoryID, &l.NextStep, &l.BoardColumn,
			&l.BoardPosition, &ttl, &created, &reviewed, &archived, &l.WallabagEntryID, &l.FetchStatus); err != nil {
			return nil, fmt.Errorf("scan link: %w", err)
		}
		l.TTLExpiresAt = parseTime(ttl)
		l.CreatedAt = parseTime(created)
		l.ReviewedAt = parseTime(reviewed)
		l.ArchivedAt = parseTime(archived)
		out = append(out, l)
	}
	return out, rows.Err()
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
