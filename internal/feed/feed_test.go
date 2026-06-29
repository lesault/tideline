package feed

import (
	"strings"
	"testing"
	"time"
)

func TestRenderProducesValidRSS(t *testing.T) {
	items := []Item{
		{
			Title:       "Read me soon",
			URL:         "https://example.com/a?x=1&y=2",
			Description: "Due in 2 days",
			GUID:        "tideline-1",
			Published:   time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC),
		},
	}
	out, err := Render("Tideline — due links", "https://pi.local/flotsam", "Links nearing expiry", items)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, want := range []string{
		`<?xml version="1.0"`,
		`<rss version="2.0"`,
		"<title>Tideline — due links</title>",
		"<item>",
		"<title>Read me soon</title>",
		"tideline-1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RSS output missing %q\n---\n%s", want, out)
		}
	}

	// The ampersand in the URL must be XML-escaped, not raw.
	if strings.Contains(out, "x=1&y=2") {
		t.Errorf("URL ampersand not escaped:\n%s", out)
	}
	if !strings.Contains(out, "x=1&amp;y=2") {
		t.Errorf("expected escaped ampersand in URL:\n%s", out)
	}
}

func TestRenderEmptyFeedStillValid(t *testing.T) {
	out, err := Render("Empty", "https://pi.local", "Nothing due", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "<rss version=\"2.0\"") || !strings.Contains(out, "<channel>") {
		t.Fatalf("empty feed is not valid RSS:\n%s", out)
	}
}
