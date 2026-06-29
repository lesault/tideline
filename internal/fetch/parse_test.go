package fetch

import "testing"

const richHTML = `<!doctype html>
<html>
<head>
  <title>Plain Title</title>
  <meta name="description" content="A plain meta description.">
  <meta property="og:title" content="Social Title">
  <meta property="og:image" content="/images/cover.png">
  <link rel="icon" href="/static/favicon.ico">
</head>
<body><h1>Hello</h1></body>
</html>`

func TestParsePrefersOGTitleAndResolvesRelativeURLs(t *testing.T) {
	m := Parse([]byte(richHTML), "https://blog.example.com/posts/1")

	if m.Title != "Social Title" {
		t.Errorf("Title = %q, want og:title %q", m.Title, "Social Title")
	}
	if m.Excerpt != "A plain meta description." {
		t.Errorf("Excerpt = %q", m.Excerpt)
	}
	if m.ImageURL != "https://blog.example.com/images/cover.png" {
		t.Errorf("ImageURL = %q, want resolved absolute", m.ImageURL)
	}
	if m.FaviconURL != "https://blog.example.com/static/favicon.ico" {
		t.Errorf("FaviconURL = %q, want resolved absolute", m.FaviconURL)
	}
	if m.Domain != "blog.example.com" {
		t.Errorf("Domain = %q", m.Domain)
	}
}

func TestParseFallsBackToTitleTagAndOGDescription(t *testing.T) {
	html := `<html><head><title>Just Title</title>
		<meta property="og:description" content="OG desc fallback."></head></html>`
	m := Parse([]byte(html), "https://x.example/a")
	if m.Title != "Just Title" {
		t.Errorf("Title = %q, want <title> fallback", m.Title)
	}
	if m.Excerpt != "OG desc fallback." {
		t.Errorf("Excerpt = %q, want og:description fallback", m.Excerpt)
	}
}

func TestParseDefaultsFaviconToRootWhenMissing(t *testing.T) {
	m := Parse([]byte(`<html><head><title>T</title></head></html>`), "https://no-icon.example/deep/page")
	if m.FaviconURL != "https://no-icon.example/favicon.ico" {
		t.Errorf("FaviconURL = %q, want default /favicon.ico", m.FaviconURL)
	}
}

func TestParseDomainFallsBackToTitleWhenNoTitleTag(t *testing.T) {
	// A bare page with no <title>: Title should be empty, not crash.
	m := Parse([]byte(`<html><body>no head</body></html>`), "https://bare.example/")
	if m.Title != "" {
		t.Errorf("Title = %q, want empty for titleless page", m.Title)
	}
	if m.Domain != "bare.example" {
		t.Errorf("Domain = %q", m.Domain)
	}
}
