// Package feed renders an RSS 2.0 feed of links that are due for attention, so a
// feed reader can nudge the user outside the app. Rendering is pure and uses
// encoding/xml for correct escaping.
package feed

import (
	"encoding/xml"
	"time"
)

// Item is one entry in the due feed.
type Item struct {
	Title       string
	URL         string
	Description string
	GUID        string
	Published   time.Time
}

type rss struct {
	XMLName xml.Name `xml:"rss"`
	Version string   `xml:"version,attr"`
	Channel channel  `xml:"channel"`
}

type channel struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	Items       []item `xml:"item"`
}

type item struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
}

// Render builds an RSS 2.0 document for the channel and items.
func Render(title, link, description string, items []Item) (string, error) {
	doc := rss{Version: "2.0", Channel: channel{Title: title, Link: link, Description: description}}
	for _, it := range items {
		doc.Channel.Items = append(doc.Channel.Items, item{
			Title:       it.Title,
			Link:        it.URL,
			Description: it.Description,
			GUID:        it.GUID,
			PubDate:     it.Published.UTC().Format(time.RFC1123Z),
		})
	}
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return xml.Header + string(body), nil
}
