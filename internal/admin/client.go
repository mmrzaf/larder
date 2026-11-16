// Package admin implements the HTTP client used by CLI commands.
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrUnreachable means the admin listener could not be reached.
var ErrUnreachable = errors.New("admin API unreachable")

// APIError is a structured admin API error response.
type APIError struct {
	Status  int
	Code    string
	Message string
}

// Error implements error.
func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("admin API error %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("admin API error %d (%s): %s", e.Status, e.Code, e.Message)
}

// Client talks to the admin listener.
type Client struct {
	base string
	http *http.Client
}

// New builds an admin client.
func New(base string, timeout time.Duration) (*Client, error) {
	if base == "" {
		return nil, fmt.Errorf("admin API URL is required")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		base: strings.TrimSuffix(base, "/"),
		http: &http.Client{Timeout: timeout},
	}, nil
}

// Get performs a GET request and decodes the JSON response.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Post performs a POST request and decodes the JSON response.
func (c *Client) Post(ctx context.Context, path string, in, out any) error {
	return c.do(ctx, http.MethodPost, path, in, out)
}

// Stats fetches the admin statistics document.
func (c *Client) Stats(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.Get(ctx, "/_larder/v1/stats", &out)
	return out, err
}

// Upstreams fetches upstream health.
func (c *Client) Upstreams(ctx context.Context) ([]json.RawMessage, error) {
	var out struct {
		Upstreams []json.RawMessage `json:"upstreams"`
	}
	err := c.Get(ctx, "/_larder/v1/upstreams", &out)
	return out.Upstreams, err
}

// SetOffline changes the offline switch.
func (c *Client) SetOffline(ctx context.Context, offline bool) error {
	body := map[string]bool{"offline": offline}
	return c.Post(ctx, "/_larder/v1/offline", body, nil)
}

// Reload asks the server to reload its configuration.
func (c *Client) Reload(ctx context.Context) error {
	return c.Post(ctx, "/_larder/v1/reload", nil, nil)
}

// Job fetches a job document.
func (c *Client) Job(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	err := c.Get(ctx, "/_larder/v1/jobs/"+id, &out)
	return out, err
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode admin request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("create admin request: %w", err)
	}
	if in != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read admin response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &payload)
		return &APIError{Status: response.StatusCode, Code: payload.Error, Message: payload.Message}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode admin response: %w", err)
		}
	}
	return nil
}

// Delete performs a DELETE request and decodes the JSON response.
func (c *Client) Delete(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodDelete, path, nil, out)
}

// CaptureStart begins a key capture and returns its token.
func (c *Client) CaptureStart(ctx context.Context, set string, from []string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.Post(ctx, "/_larder/v1/capture/start", map[string]any{"set": set, "from": from}, &out)
	return out.Token, err
}

// CaptureStop ends a capture and optionally pins its keys.
func (c *Client) CaptureStop(ctx context.Context, token string, pin bool) (string, int, int, error) {
	var out struct {
		Set    string `json:"set"`
		Keys   int    `json:"keys"`
		Pinned int    `json:"pinned"`
	}
	err := c.Post(ctx, "/_larder/v1/capture/stop", map[string]any{"token": token, "pin": pin}, &out)
	return out.Set, out.Keys, out.Pinned, err
}
