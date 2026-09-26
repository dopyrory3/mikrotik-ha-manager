// Package routeros is a minimal client for the RouterOS 7 REST API
// (/rest/...), used for both read (dashboard, drift) and write (sync,
// runtime deploy, failover) operations.
package routeros

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Client talks to one router's REST API over HTTPS with basic auth.
type Client struct {
	baseURL  string
	user     string
	password string
	http     *http.Client
}

// Config configures a Client for one router.
type Config struct {
	Host string
	// Port overrides the REST API (www-ssl) port. Zero means the standard
	// HTTPS port (443); set it when a router's www-ssl service has been
	// moved to a non-standard port. It is unrelated to the legacy binary
	// API service (ports 8728/8729), which this client does not speak.
	Port        int
	User        string
	Password    string
	InsecureTLS bool
	Timeout     time.Duration
}

// New builds a Client for a single router.
func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	transport := &http.Transport{}
	if cfg.InsecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit per-router opt-in, see project.md §5.1
	}

	host := cfg.Host
	if cfg.Port != 0 {
		host = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	}

	return &Client{
		baseURL:  fmt.Sprintf("https://%s/rest", host),
		user:     cfg.User,
		password: cfg.Password,
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
	}
}

// Get decodes the JSON response of a GET against path into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Post issues a PUT-equivalent create against path with body, decoding the
// response into out if non-nil.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPut, path, body, out)
}

// Patch partially updates the resource at path.
func (c *Client) Patch(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPatch, path, body, out)
}

// Delete removes the resource at path.
func (c *Client) Delete(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		reqBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(c.user, c.password)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}

	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, string(respBody))
	}

	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}
