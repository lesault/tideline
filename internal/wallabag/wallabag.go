// Package wallabag is a minimal client for the Wallabag REST API. It speaks the
// OAuth2 password grant and creates entries, working against both self-hosted
// instances and the hosted service (app.wallabag.it) — only the base URL differs.
package wallabag

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config is a user's Wallabag credentials.
type Config struct {
	BaseURL      string
	ClientID     string
	ClientSecret string
	Username     string
	Password     string
}

// Client talks to a Wallabag instance.
type Client struct {
	http *http.Client
}

// New returns a Client whose requests time out after the given duration.
func New(timeout time.Duration) *Client {
	return &Client{http: &http.Client{Timeout: timeout}}
}

// Archive authenticates with cfg and creates an entry for rawURL, returning the
// new Wallabag entry id. Authentication and entry creation are separate steps so
// failures are reported distinctly and the caller can retry without data loss.
func (c *Client) Archive(ctx context.Context, cfg Config, rawURL string) (int64, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	token, err := c.authenticate(ctx, base, cfg)
	if err != nil {
		return 0, err
	}
	return c.createEntry(ctx, base, token, rawURL)
}

func (c *Client) authenticate(ctx context.Context, base string, cfg Config) (string, error) {
	form := url.Values{
		"grant_type":    {"password"},
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"username":      {cfg.Username},
		"password":      {cfg.Password},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/oauth/v2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("wallabag auth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("wallabag auth: status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("wallabag auth: decode: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("wallabag auth: empty access token")
	}
	return out.AccessToken, nil
}

func (c *Client) createEntry(ctx context.Context, base, token, rawURL string) (int64, error) {
	form := url.Values{"url": {rawURL}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/entries.json", strings.NewReader(form.Encode()))
	if err != nil {
		return 0, fmt.Errorf("build entry request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("wallabag entry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("wallabag entry: status %d", resp.StatusCode)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("wallabag entry: decode: %w", err)
	}
	return out.ID, nil
}
