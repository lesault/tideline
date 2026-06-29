package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newTestStore opens a fresh migrated store backed by a temp file.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenRunsMigrations(t *testing.T) {
	s := newTestStore(t)
	// If migrations ran, the users table exists and is queryable.
	if _, err := s.UserByEmail(context.Background(), "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on empty users table, got %v", err)
	}
}

func TestCreateAndFetchUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	u, err := s.CreateUser(ctx, "alice@example.com", "hash123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.ID == 0 {
		t.Fatal("expected a non-zero user id")
	}

	got, err := s.UserByEmail(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	if got.ID != u.ID || got.PasswordHash != "hash123" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestCreateUserRejectsDuplicateEmail(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "dup@example.com", "h"); err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}
	if _, err := s.CreateUser(ctx, "dup@example.com", "h2"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate, got %v", err)
	}
}

func TestCreateLinkAndListInboxOrdersBySoonestExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "bob@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Insert out of order; expect ListInbox sorted by soonest expiry first.
	later, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://late.example", CreatedAt: base, TTLExpiresAt: base.Add(10 * 24 * time.Hour)})
	sooner, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://soon.example", CreatedAt: base, TTLExpiresAt: base.Add(2 * 24 * time.Hour)})

	got, err := s.ListInbox(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 inbox links, got %d", len(got))
	}
	if got[0].ID != sooner.ID || got[1].ID != later.ID {
		t.Fatalf("inbox not sorted by soonest expiry: %v then %v", got[0].URL, got[1].URL)
	}
}

func TestListInboxIsScopedPerUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	alice, _ := s.CreateUser(ctx, "a@example.com", "h")
	mallory, _ := s.CreateUser(ctx, "m@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.CreateLink(ctx, Link{UserID: alice.ID, URL: "https://alice.example", CreatedAt: base, TTLExpiresAt: base.Add(time.Hour)})

	got, err := s.ListInbox(ctx, mallory.ID)
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("mallory should see no links, got %d", len(got))
	}
}

func TestCreateUserSeedsDefaultCategories(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "seed@example.com", "h")

	cats, err := s.ListCategories(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	got := map[string]bool{}
	for _, c := range cats {
		got[c.Name] = true
	}
	for _, want := range []string{"Tech", "Read", "Reference", "Fun"} {
		if !got[want] {
			t.Fatalf("missing seeded category %q; got %v", want, got)
		}
	}
}

func TestCreateCategoryIsScopedAndUnique(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "cat@example.com", "h")

	c, err := s.CreateCategory(ctx, u.ID, "Cooking")
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	if c.ID == 0 || c.Name != "Cooking" {
		t.Fatalf("unexpected category: %+v", c)
	}
	if _, err := s.CreateCategory(ctx, u.ID, "Cooking"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate for same name, got %v", err)
	}
}

func TestTriageMovesLinkToBoard(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "tri@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://x.example", CreatedAt: base, TTLExpiresAt: base.Add(48 * time.Hour)})
	cats, _ := s.ListCategories(ctx, u.ID)
	catID := cats[0].ID

	if err := s.TriageLink(ctx, u.ID, l.ID, &catID, "read"); err != nil {
		t.Fatalf("TriageLink: %v", err)
	}
	got, _ := s.LinkByID(ctx, l.ID)
	if got.Status != StatusTriaged {
		t.Fatalf("status = %q, want triaged", got.Status)
	}
	if got.BoardColumn != ColReviewing {
		t.Fatalf("board column = %q, want %q", got.BoardColumn, ColReviewing)
	}
	if got.NextStep != "read" || got.CategoryID == nil || *got.CategoryID != catID {
		t.Fatalf("triage fields not set: %+v", got)
	}
	if got.ReviewedAt.IsZero() {
		t.Fatal("reviewed_at should be set")
	}
	// Triaged links leave the inbox, so a sweep never touches them.
	if n, _ := s.SweepExpired(ctx, base.Add(72*time.Hour)); n != 0 {
		t.Fatalf("triaged link should not be swept, swept %d", n)
	}
}

func TestTriageIsScopedPerUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	owner, _ := s.CreateUser(ctx, "owner@example.com", "h")
	other, _ := s.CreateUser(ctx, "other@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l, _ := s.CreateLink(ctx, Link{UserID: owner.ID, URL: "https://x.example", CreatedAt: base, TTLExpiresAt: base.Add(time.Hour)})

	if err := s.TriageLink(ctx, other.ID, l.ID, nil, "read"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("triaging another user's link should be ErrNotFound, got %v", err)
	}
}

func TestDropLink(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "drop@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://x.example", CreatedAt: base, TTLExpiresAt: base.Add(time.Hour)})

	if err := s.DropLink(ctx, u.ID, l.ID); err != nil {
		t.Fatalf("DropLink: %v", err)
	}
	got, _ := s.LinkByID(ctx, l.ID)
	if got.Status != StatusDropped {
		t.Fatalf("status = %q, want dropped", got.Status)
	}
}

func TestMoveCardAndListBoard(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "board@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://a.example", CreatedAt: base, TTLExpiresAt: base.Add(time.Hour)})
	b, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://b.example", CreatedAt: base, TTLExpiresAt: base.Add(time.Hour)})
	s.TriageLink(ctx, u.ID, a.ID, nil, "read")
	s.TriageLink(ctx, u.ID, b.ID, nil, "schedule")

	if err := s.MoveCard(ctx, u.ID, a.ID, ColNext, 0); err != nil {
		t.Fatalf("MoveCard: %v", err)
	}
	got, _ := s.LinkByID(ctx, a.ID)
	if got.BoardColumn != ColNext {
		t.Fatalf("card a column = %q, want %q", got.BoardColumn, ColNext)
	}

	board, err := s.ListBoard(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListBoard: %v", err)
	}
	if len(board) != 2 {
		t.Fatalf("board should hold 2 cards, got %d", len(board))
	}

	// Moving to an unknown column is rejected.
	if err := s.MoveCard(ctx, u.ID, a.ID, "Nonsense", 0); err == nil {
		t.Fatal("expected error moving to an invalid column")
	}
}

func TestUpdateMetadataFillsPreviewAndFetchStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "d@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://x.example", CreatedAt: base, TTLExpiresAt: base.Add(time.Hour)})
	if l.FetchStatus != "pending" {
		t.Fatalf("new link should be pending, got %q", l.FetchStatus)
	}

	err := s.UpdateMetadata(ctx, l.ID, Metadata{
		Title: "T", Excerpt: "E", ImageURL: "https://x.example/i.png",
		FaviconURL: "https://x.example/f.ico", Domain: "x.example",
	}, "ok")
	if err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}

	got, _ := s.LinkByID(ctx, l.ID)
	if got.Title != "T" || got.Excerpt != "E" || got.Domain != "x.example" || got.FetchStatus != "ok" {
		t.Fatalf("metadata not persisted: %+v", got)
	}
}

func TestSweepExpiredMovesOnlyExpiredInboxItems(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "c@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base.Add(5 * 24 * time.Hour)

	expired, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://old.example", CreatedAt: base, TTLExpiresAt: base.Add(24 * time.Hour)})
	fresh, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://new.example", CreatedAt: base, TTLExpiresAt: base.Add(10 * 24 * time.Hour)})

	n, err := s.SweepExpired(ctx, now)
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 swept, got %d", n)
	}

	gotExpired, _ := s.LinkByID(ctx, expired.ID)
	if gotExpired.Status != StatusFlotsam {
		t.Fatalf("expired link should be in flotsam, got %q", gotExpired.Status)
	}
	gotFresh, _ := s.LinkByID(ctx, fresh.ID)
	if gotFresh.Status != StatusInbox {
		t.Fatalf("fresh link should remain in inbox, got %q", gotFresh.Status)
	}

	// Sweeping again is a no-op (idempotent): already-flotsam items aren't re-swept.
	if n2, _ := s.SweepExpired(ctx, now); n2 != 0 {
		t.Fatalf("second sweep should move 0, got %d", n2)
	}
}
