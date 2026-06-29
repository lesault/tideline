// Package fetch turns a captured URL into display metadata (title, excerpt,
// image, favicon, domain). Parsing is pure and tested against HTML fixtures;
// the network Fetch is a thin wrapper so capture never blocks on it.
package fetch

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// Metadata is the lightweight preview Tideline stores for a link. It never
// includes article body text — archiving full content is Wallabag's job.
type Metadata struct {
	Title      string
	Excerpt    string
	ImageURL   string
	FaviconURL string
	Domain     string
}

// Parse extracts Metadata from an HTML document fetched from pageURL. Relative
// image/favicon URLs are resolved against pageURL; a missing favicon defaults
// to /favicon.ico at the page's host.
func Parse(body []byte, pageURL string) Metadata {
	var m Metadata
	base, _ := url.Parse(pageURL)
	if base != nil {
		m.Domain = base.Host
	}

	var titleTag, ogTitle, metaDesc, ogDesc, ogImage, favicon string

	doc, err := html.Parse(bytes.NewReader(body))
	if err == nil {
		var walk func(*html.Node)
		walk = func(n *html.Node) {
			if n.Type == html.ElementNode {
				switch n.Data {
				case "title":
					if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
						titleTag = strings.TrimSpace(n.FirstChild.Data)
					}
				case "meta":
					name := strings.ToLower(attr(n, "name"))
					prop := strings.ToLower(attr(n, "property"))
					content := strings.TrimSpace(attr(n, "content"))
					switch {
					case name == "description":
						metaDesc = content
					case prop == "og:title":
						ogTitle = content
					case prop == "og:description":
						ogDesc = content
					case prop == "og:image":
						ogImage = content
					}
				case "link":
					rel := strings.ToLower(attr(n, "rel"))
					if favicon == "" && strings.Contains(rel, "icon") {
						favicon = strings.TrimSpace(attr(n, "href"))
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(doc)
	}

	m.Title = firstNonEmpty(ogTitle, titleTag)
	m.Excerpt = firstNonEmpty(metaDesc, ogDesc)
	m.ImageURL = resolve(base, ogImage)

	if favicon != "" {
		m.FaviconURL = resolve(base, favicon)
	} else if base != nil {
		m.FaviconURL = resolve(base, "/favicon.ico")
	}
	return m
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// resolve turns a possibly-relative href into an absolute URL against base.
func resolve(base *url.URL, href string) string {
	if href == "" || base == nil {
		return href
	}
	ref, err := url.Parse(href)
	if err != nil {
		return href
	}
	return base.ResolveReference(ref).String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
