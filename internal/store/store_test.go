package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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

func TestUpdateDefaultTTL(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "ttl@example.com", "h")
	if u.DefaultTTLDays != 14 {
		t.Fatalf("new account default TTL = %d, want 14", u.DefaultTTLDays)
	}

	if err := s.UpdateDefaultTTL(ctx, u.ID, 30); err != nil {
		t.Fatalf("UpdateDefaultTTL: %v", err)
	}
	got, _ := s.UserByID(ctx, u.ID)
	if got.DefaultTTLDays != 30 {
		t.Fatalf("default TTL = %d, want 30", got.DefaultTTLDays)
	}
}

func TestUpdateThemeRoundTrips(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "theme@example.com", "h")
	if u.Theme != "" {
		t.Fatalf("new account theme = %q, want empty (follow OS)", u.Theme)
	}

	if err := s.UpdateTheme(ctx, u.ID, "dark"); err != nil {
		t.Fatalf("UpdateTheme: %v", err)
	}
	got, _ := s.UserByID(ctx, u.ID)
	if got.Theme != "dark" {
		t.Fatalf("theme = %q, want %q", got.Theme, "dark")
	}

	// Scoped: updating a missing user yields ErrNotFound.
	if err := s.UpdateTheme(ctx, u.ID+999, "light"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateTheme on unknown user should be ErrNotFound, got %v", err)
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
	for _, want := range []string{"Tech", "Read", "Fun"} {
		if !got[want] {
			t.Fatalf("missing seeded category %q; got %v", want, got)
		}
	}
	// "Reference" is now a board column, not a seeded category.
	if got["Reference"] {
		t.Fatalf("Reference should not be seeded as a category; got %v", got)
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

	if err := s.TriageLink(ctx, u.ID, l.ID, TriageInput{CategoryID: &catID, NextStep: "read"}); err != nil {
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

	if err := s.TriageLink(ctx, other.ID, l.ID, TriageInput{NextStep: "read"}); !errors.Is(err, ErrNotFound) {
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
	s.TriageLink(ctx, u.ID, a.ID, TriageInput{NextStep: "read"})
	s.TriageLink(ctx, u.ID, b.ID, TriageInput{NextStep: "schedule"})

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

func TestReferenceLinkSetsStatusAndStampsReviewed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "ref@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://ref.example", CreatedAt: base, TTLExpiresAt: base.Add(48 * time.Hour)})

	if err := s.ReferenceLink(ctx, u.ID, l.ID); err != nil {
		t.Fatalf("ReferenceLink: %v", err)
	}
	got, _ := s.LinkByID(ctx, l.ID)
	if got.Status != StatusReference {
		t.Fatalf("status = %q, want %q", got.Status, StatusReference)
	}
	if got.ReviewedAt.IsZero() {
		t.Fatal("reviewed_at should be set")
	}

	// Scoped: another user can't reference this link.
	other, _ := s.CreateUser(ctx, "nope@example.com", "h")
	if err := s.ReferenceLink(ctx, other.ID, l.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user ReferenceLink should be ErrNotFound, got %v", err)
	}
}

func TestUpdateNotesRoundTripsAndIsScoped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "notes@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://n.example", CreatedAt: base, TTLExpiresAt: base.Add(48 * time.Hour)})

	// New links start with empty notes.
	if l0, _ := s.LinkByID(ctx, l.ID); l0.Notes != "" {
		t.Fatalf("new link notes = %q, want empty", l0.Notes)
	}

	if err := s.UpdateNotes(ctx, u.ID, l.ID, "remember this snippet"); err != nil {
		t.Fatalf("UpdateNotes: %v", err)
	}
	got, _ := s.LinkByID(ctx, l.ID)
	if got.Notes != "remember this snippet" {
		t.Fatalf("notes = %q, want %q", got.Notes, "remember this snippet")
	}

	// Scoped: another user can't edit notes.
	other, _ := s.CreateUser(ctx, "nope2@example.com", "h")
	if err := s.UpdateNotes(ctx, other.ID, l.ID, "hijack"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user UpdateNotes should be ErrNotFound, got %v", err)
	}
}

func TestListReferenceFiltersByStatusAndQuery(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "list@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	mk := func(url, title string) int64 {
		l, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: url, Title: title, CreatedAt: base, TTLExpiresAt: base.Add(48 * time.Hour)})
		return l.ID
	}

	// Two reference links and one inbox link.
	a := mk("https://a.example", "Gopher Guide")
	b := mk("https://b.example", "Rust Manual")
	inbox := mk("https://c.example", "Stays in inbox")
	s.ReferenceLink(ctx, u.ID, a)
	s.ReferenceLink(ctx, u.ID, b)
	s.UpdateNotes(ctx, u.ID, a, "useful kubernetes recipe")
	_ = inbox

	// No query -> only reference-status links.
	all, err := s.ListReference(ctx, u.ID, "")
	if err != nil {
		t.Fatalf("ListReference: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 reference links, got %d", len(all))
	}

	// Query matching a word only present in notes.
	byNotes, _ := s.ListReference(ctx, u.ID, "kubernetes")
	if len(byNotes) != 1 || byNotes[0].ID != a {
		t.Fatalf("notes query want [%d], got %+v", a, byNotes)
	}

	// Query matching a title.
	byTitle, _ := s.ListReference(ctx, u.ID, "rust")
	if len(byTitle) != 1 || byTitle[0].ID != b {
		t.Fatalf("title query want [%d], got %+v", b, byTitle)
	}
}

func TestReferenceLinkExcludedFromInboxBoardAndSweep(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "excl@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://ex.example", CreatedAt: base, TTLExpiresAt: base.Add(24 * time.Hour)})
	if err := s.ReferenceLink(ctx, u.ID, l.ID); err != nil {
		t.Fatalf("ReferenceLink: %v", err)
	}

	if inbox, _ := s.ListInbox(ctx, u.ID); len(inbox) != 0 {
		t.Fatalf("reference link should not appear in inbox, got %d", len(inbox))
	}
	if board, _ := s.ListBoard(ctx, u.ID); len(board) != 0 {
		t.Fatalf("reference link should not appear on board, got %d", len(board))
	}
	if n, _ := s.SweepExpired(ctx, base.Add(72*time.Hour)); n != 0 {
		t.Fatalf("reference link should not be swept, swept %d", n)
	}
	got, _ := s.LinkByID(ctx, l.ID)
	if got.Status != StatusReference {
		t.Fatalf("status = %q, want %q", got.Status, StatusReference)
	}
}

func TestTriagePersistsScheduledFor(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "sched@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://s.example", CreatedAt: base, TTLExpiresAt: base.Add(48 * time.Hour)})

	when := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)
	if err := s.TriageLink(ctx, u.ID, l.ID, TriageInput{NextStep: "read", ScheduledFor: when}); err != nil {
		t.Fatalf("TriageLink: %v", err)
	}
	got, _ := s.LinkByID(ctx, l.ID)
	if !got.ScheduledFor.Equal(when) {
		t.Fatalf("scheduled_for = %v, want %v", got.ScheduledFor, when)
	}

	// A triage with no schedule leaves scheduled_for zero.
	l2, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://s2.example", CreatedAt: base, TTLExpiresAt: base.Add(48 * time.Hour)})
	if err := s.TriageLink(ctx, u.ID, l2.ID, TriageInput{NextStep: "read"}); err != nil {
		t.Fatalf("TriageLink: %v", err)
	}
	got2, _ := s.LinkByID(ctx, l2.ID)
	if !got2.ScheduledFor.IsZero() {
		t.Fatalf("scheduled_for = %v, want zero", got2.ScheduledFor)
	}
}

func TestScheduledDueReturnsOnlyDueLinks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "due@example.com", "h")
	other, _ := s.CreateUser(ctx, "other@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)

	mk := func(owner int64, url string) int64 {
		l, _ := s.CreateLink(ctx, Link{UserID: owner, URL: url, CreatedAt: base, TTLExpiresAt: base.Add(48 * time.Hour)})
		return l.ID
	}

	// Due: scheduled before now.
	dueID := mk(u.ID, "https://due.example")
	s.TriageLink(ctx, u.ID, dueID, TriageInput{NextStep: "read", ScheduledFor: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)})
	// Future: scheduled after now -> not due.
	futID := mk(u.ID, "https://future.example")
	s.TriageLink(ctx, u.ID, futID, TriageInput{NextStep: "read", ScheduledFor: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)})
	// Unscheduled: no schedule -> not due.
	unschedID := mk(u.ID, "https://unsched.example")
	s.TriageLink(ctx, u.ID, unschedID, TriageInput{NextStep: "read"})
	// Other user's due link -> not visible.
	otherID := mk(other.ID, "https://otherdue.example")
	s.TriageLink(ctx, other.ID, otherID, TriageInput{NextStep: "read", ScheduledFor: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)})

	got, err := s.ScheduledDue(ctx, u.ID, now)
	if err != nil {
		t.Fatalf("ScheduledDue: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 due link, got %d: %+v", len(got), got)
	}
	if got[0].ID != dueID {
		t.Fatalf("due link = %d, want %d", got[0].ID, dueID)
	}
}

func TestRestoreToInboxReArmsDecayAndClearsBoardFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "restore@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cats, _ := s.ListCategories(ctx, u.ID)
	catID := cats[0].ID

	l, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://r.example", CreatedAt: base, TTLExpiresAt: base.Add(24 * time.Hour)})
	// Triage it onto the board with category, next-step, and a schedule, give it notes, then drop it.
	when := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)
	if err := s.TriageLink(ctx, u.ID, l.ID, TriageInput{CategoryID: &catID, NextStep: "read", Column: ColNext, ScheduledFor: when}); err != nil {
		t.Fatalf("TriageLink: %v", err)
	}
	if err := s.UpdateNotes(ctx, u.ID, l.ID, "keep me"); err != nil {
		t.Fatalf("UpdateNotes: %v", err)
	}
	if err := s.DropLink(ctx, u.ID, l.ID); err != nil {
		t.Fatalf("DropLink: %v", err)
	}

	newExpiry := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	if err := s.RestoreToInbox(ctx, u.ID, l.ID, newExpiry); err != nil {
		t.Fatalf("RestoreToInbox: %v", err)
	}

	got, _ := s.LinkByID(ctx, l.ID)
	if got.Status != StatusInbox {
		t.Fatalf("status = %q, want inbox", got.Status)
	}
	if !got.TTLExpiresAt.Equal(newExpiry) {
		t.Fatalf("ttl_expires_at = %v, want %v", got.TTLExpiresAt, newExpiry)
	}
	if got.BoardColumn != "" {
		t.Fatalf("board_column = %q, want empty", got.BoardColumn)
	}
	if !got.ScheduledFor.IsZero() {
		t.Fatalf("scheduled_for = %v, want zero", got.ScheduledFor)
	}
	if got.NextStep != "" {
		t.Fatalf("next_step = %q, want empty", got.NextStep)
	}
	// category_id and notes are preserved.
	if got.CategoryID == nil || *got.CategoryID != catID {
		t.Fatalf("category_id = %v, want %d", got.CategoryID, catID)
	}
	if got.Notes != "keep me" {
		t.Fatalf("notes = %q, want %q", got.Notes, "keep me")
	}

	// Scoped: another user can't restore this link.
	other, _ := s.CreateUser(ctx, "nope-ri@example.com", "h")
	if err := s.RestoreToInbox(ctx, other.ID, l.ID, newExpiry); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user RestoreToInbox should be ErrNotFound, got %v", err)
	}
}

func TestRestoreToBoardReturnsReferenceToBoard(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "rb@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://rb.example", CreatedAt: base, TTLExpiresAt: base.Add(24 * time.Hour)})
	if err := s.UpdateNotes(ctx, u.ID, l.ID, "still useful"); err != nil {
		t.Fatalf("UpdateNotes: %v", err)
	}
	if err := s.ReferenceLink(ctx, u.ID, l.ID); err != nil {
		t.Fatalf("ReferenceLink: %v", err)
	}

	if err := s.RestoreToBoard(ctx, u.ID, l.ID, ColReviewing); err != nil {
		t.Fatalf("RestoreToBoard: %v", err)
	}
	got, _ := s.LinkByID(ctx, l.ID)
	if got.Status != StatusTriaged {
		t.Fatalf("status = %q, want triaged", got.Status)
	}
	if got.BoardColumn != ColReviewing {
		t.Fatalf("board_column = %q, want %q", got.BoardColumn, ColReviewing)
	}
	if got.Notes != "still useful" {
		t.Fatalf("notes = %q, want preserved", got.Notes)
	}

	// An invalid column is rejected (not a silent no-op).
	if err := s.RestoreToBoard(ctx, u.ID, l.ID, "Nonsense"); err == nil {
		t.Fatal("expected error restoring to an invalid column")
	}

	// Scoped: another user can't restore this link.
	other, _ := s.CreateUser(ctx, "nope-rb@example.com", "h")
	if err := s.RestoreToBoard(ctx, other.ID, l.ID, ColReviewing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user RestoreToBoard should be ErrNotFound, got %v", err)
	}
}

func TestAPITokenCreateResolveListDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "tok@example.com", "h")

	plain, tok, err := s.CreateAPIToken(ctx, u.ID, "capture", "Firefox")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if plain == "" || tok.ID == 0 || tok.Scope != "capture" || tok.Label != "Firefox" {
		t.Fatalf("unexpected token: plain=%q tok=%+v", plain, tok)
	}

	// The raw token resolves to its owner and scope.
	got, err := s.APITokenByValue(ctx, plain)
	if err != nil {
		t.Fatalf("APITokenByValue: %v", err)
	}
	if got.UserID != u.ID || got.Scope != "capture" {
		t.Fatalf("resolved token mismatch: %+v", got)
	}

	// A wrong value does not resolve.
	if _, err := s.APITokenByValue(ctx, "not-a-real-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bogus token should be ErrNotFound, got %v", err)
	}

	// Listing never exposes the secret.
	list, _ := s.ListAPITokens(ctx, u.ID)
	if len(list) != 1 || list[0].Label != "Firefox" {
		t.Fatalf("ListAPITokens = %+v", list)
	}

	// Deletion is scoped and revokes the token.
	if err := s.DeleteAPIToken(ctx, u.ID, tok.ID); err != nil {
		t.Fatalf("DeleteAPIToken: %v", err)
	}
	if _, err := s.APITokenByValue(ctx, plain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted token should no longer resolve, got %v", err)
	}
}

func TestAPITokenDeleteIsScopedPerUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	owner, _ := s.CreateUser(ctx, "o@example.com", "h")
	other, _ := s.CreateUser(ctx, "x@example.com", "h")
	_, tok, _ := s.CreateAPIToken(ctx, owner.ID, "feed", "reader")

	if err := s.DeleteAPIToken(ctx, other.ID, tok.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting another user's token should be ErrNotFound, got %v", err)
	}
}

func TestWallabagAccountSaveLoadAndUpsert(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "wb@example.com", "h")

	if _, err := s.WallabagAccount(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound before any account, got %v", err)
	}

	acct := WallabagAccount{
		UserID: u.ID, BaseURL: "https://wb.example", ClientID: "cid",
		ClientSecret: "sec", Username: "me", Password: "pw",
	}
	if err := s.SaveWallabagAccount(ctx, acct); err != nil {
		t.Fatalf("SaveWallabagAccount: %v", err)
	}
	got, err := s.WallabagAccount(ctx, u.ID)
	if err != nil {
		t.Fatalf("WallabagAccount: %v", err)
	}
	if got.BaseURL != "https://wb.example" || got.ClientID != "cid" || got.Password != "pw" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// Saving again updates in place (one account per user).
	acct.BaseURL = "https://changed.example"
	if err := s.SaveWallabagAccount(ctx, acct); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	got, _ = s.WallabagAccount(ctx, u.ID)
	if got.BaseURL != "https://changed.example" {
		t.Fatalf("upsert did not update base_url: %+v", got)
	}
}

func TestWallabagCredsEncryptedAtRest(t *testing.T) {
	s := newTestStore(t)
	s.SetSecret("a-strong-instance-secret")
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "enc@example.com", "h")

	in := WallabagAccount{UserID: u.ID, BaseURL: "https://wb.example", ClientID: "cid",
		ClientSecret: "shhh", Username: "me", Password: "pw"}
	if err := s.SaveWallabagAccount(ctx, in); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Raw DB columns must be ciphertext, not the plaintext secrets.
	var rawPw, rawSecret string
	s.db.QueryRowContext(ctx, `SELECT password, client_secret FROM wallabag_accounts WHERE user_id=?`, u.ID).Scan(&rawPw, &rawSecret)
	if rawPw == "pw" || rawSecret == "shhh" {
		t.Fatalf("secrets stored as plaintext: pw=%q secret=%q", rawPw, rawSecret)
	}
	if !strings.HasPrefix(rawPw, encPrefix) || !strings.HasPrefix(rawSecret, encPrefix) {
		t.Fatalf("expected encrypted columns, got pw=%q secret=%q", rawPw, rawSecret)
	}

	// Loading decrypts back to the originals.
	got, _ := s.WallabagAccount(ctx, u.ID)
	if got.Password != "pw" || got.ClientSecret != "shhh" {
		t.Fatalf("decrypt round-trip failed: %+v", got)
	}
}

func TestWallabagPlaintextWhenNoSecret(t *testing.T) {
	s := newTestStore(t) // no SetSecret
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "plain@example.com", "h")
	s.SaveWallabagAccount(ctx, WallabagAccount{UserID: u.ID, BaseURL: "b", ClientID: "c", ClientSecret: "shhh", Username: "u", Password: "pw"})

	var rawPw string
	s.db.QueryRowContext(ctx, `SELECT password FROM wallabag_accounts WHERE user_id=?`, u.ID).Scan(&rawPw)
	if rawPw != "pw" {
		t.Fatalf("without a secret, password should be plaintext, got %q", rawPw)
	}
	got, _ := s.WallabagAccount(ctx, u.ID)
	if got.Password != "pw" {
		t.Fatalf("load = %q, want pw", got.Password)
	}
}

func TestWallabagLegacyPlaintextReadsWithSecret(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "legacy@example.com", "h")
	// Save while encryption is OFF (simulates a pre-encryption row)...
	s.SaveWallabagAccount(ctx, WallabagAccount{UserID: u.ID, BaseURL: "b", ClientID: "c", ClientSecret: "shhh", Username: "u", Password: "pw"})
	// ...then turn encryption on and read: the legacy plaintext must still load.
	s.SetSecret("now-encrypting")
	got, err := s.WallabagAccount(ctx, u.ID)
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	if got.Password != "pw" || got.ClientSecret != "shhh" {
		t.Fatalf("legacy plaintext didn't load: %+v", got)
	}
}

func TestArchiveLinkRecordsWallabagEntry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "arch@example.com", "h")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l, _ := s.CreateLink(ctx, Link{UserID: u.ID, URL: "https://x.example", CreatedAt: base, TTLExpiresAt: base.Add(time.Hour)})
	s.TriageLink(ctx, u.ID, l.ID, TriageInput{NextStep: "read"})

	if err := s.ArchiveLink(ctx, u.ID, l.ID, 4242); err != nil {
		t.Fatalf("ArchiveLink: %v", err)
	}
	got, _ := s.LinkByID(ctx, l.ID)
	if got.Status != StatusArchived {
		t.Fatalf("status = %q, want archived", got.Status)
	}
	if got.WallabagEntryID == nil || *got.WallabagEntryID != 4242 {
		t.Fatalf("wallabag entry id not recorded: %+v", got.WallabagEntryID)
	}
	if got.ArchivedAt.IsZero() {
		t.Fatal("archived_at should be set")
	}

	// Scoped: another user can't archive this link.
	other, _ := s.CreateUser(ctx, "nope@example.com", "h")
	if err := s.ArchiveLink(ctx, other.ID, l.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user archive should be ErrNotFound, got %v", err)
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
