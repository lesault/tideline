// Package fetch turns a captured URL into display metadata (title, excerpt,
// image, favicon, domain). Parsing is pure and tested against HTML fixtures;
// the network Fetch is a thin wrapper so capture never blocks on it.
package fetch

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// maxBody caps how much of a page we read — metadata lives in <head>, so a few
// hundred KB is plenty and protects the Pi from huge documents.
const maxBody = 512 * 1024

// Fetcher retrieves and parses link metadata over HTTP.
type Fetcher struct {
	client *http.Client
}

// New returns a Fetcher safe for untrusted, user-supplied URLs. It refuses to
// connect to non-public addresses (loopback, RFC1918/ULA private, link-local
// including the 169.254.169.254 cloud-metadata endpoint, unspecified, and
// multicast), enforced at dial time on the initial request AND every redirect
// hop — so neither a redirect nor DNS rebinding can reach internal services.
// This is the SSRF guard.
func New(timeout time.Duration) *Fetcher { return newFetcher(timeout, true) }

// NewAllowingPrivate returns a Fetcher that may connect to private/loopback
// addresses. Use ONLY with trusted URLs (e.g. tests) — never with user input.
func NewAllowingPrivate(timeout time.Duration) *Fetcher { return newFetcher(timeout, false) }

func newFetcher(timeout time.Duration, blockPrivate bool) *Fetcher {
	dialer := &net.Dialer{Timeout: timeout}
	if blockPrivate {
		// Control runs after DNS resolution with the concrete IP:port about to
		// be dialed, for every connection the client makes (including redirects),
		// which is what makes the check rebinding-safe.
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("ssrf guard: malformed address %q: %w", address, err)
			}
			ip := net.ParseIP(host)
			if ip == nil || isBlockedIP(ip) {
				return fmt.Errorf("ssrf guard: refusing to connect to non-public address %s", host)
			}
			return nil
		}
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	return &Fetcher{client: &http.Client{Timeout: timeout, Transport: transport}}
}

// isBlockedIP reports whether ip is anything other than a routable public
// address.
func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// Fetch GETs url and parses its metadata. It returns an error on transport
// failure, a blocked (non-public) address, or any non-2xx status, leaving the
// caller to mark the link's fetch status failed without losing the capture.
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
