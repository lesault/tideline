package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxBody caps how much of a page we read — metadata lives in <head>, so a few
// hundred KB is plenty and protects the Pi from huge documents.
const maxBody = 512 * 1024

// Fetcher retrieves and parses link metadata over HTTP.
type Fetcher struct {
	client *http.Client
}

// New returns a Fetcher whose requests time out after the given duration.
func New(timeout time.Duration) *Fetcher {
	return &Fetcher{client: &http.Client{Timeout: timeout}}
}

// Fetch GETs url and parses its metadata. It returns an error on transport
// failure or any non-2xx status, leaving the caller to mark the link's fetch
// status failed without losing the capture.
func (f *Fetcher) Fetch(ctx context.Context, url string) (Metadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Metadata{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Tideline/1.0 (+https://github.com/lesault/tideline)")

	resp, err := f.client.Do(req)
	if err != nil {
		return Metadata{}, fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Metadata{}, fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Metadata{}, fmt.Errorf("read body: %w", err)
	}
	return Parse(body, url), nil
}
