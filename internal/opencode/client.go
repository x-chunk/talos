// Package opencode is a client for the OpenCode 2.x HTTP API.
//
// It covers only what Talos needs. The verified contract, including the parts
// OpenCode does not document, lives in docs/opencode-api.md.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// basicAuthUser is the fixed username OpenCode expects for HTTP Basic auth.
const basicAuthUser = "opencode"

// Endpoint says where a server listens and how to authenticate to it.
type Endpoint struct {
	URL      string
	Password string
}

// Client calls one OpenCode server. It is safe for concurrent use.
type Client struct {
	http *http.Client

	mu       sync.RWMutex
	endpoint Endpoint
}

// NewClient returns a client for endpoint. A nil httpClient means
// http.DefaultClient, which must not have a timeout if the client will read
// the event stream.
func NewClient(endpoint Endpoint, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{http: httpClient, endpoint: endpoint}
}

// Endpoint returns the endpoint currently in use.
func (c *Client) Endpoint() Endpoint {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.endpoint
}

// SetEndpoint replaces the endpoint for later requests, for example after the
// service restarted with a new password. Requests in flight are unaffected.
func (c *Client) SetEndpoint(endpoint Endpoint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endpoint = endpoint
}

// APIError is a non-2xx response from OpenCode. Tag is the `_tag` of the error
// body, such as "SessionNotFoundError", and is empty if the body was not JSON.
type APIError struct {
	Status  int
	Tag     string
	Message string
}

func (e *APIError) Error() string {
	if e.Tag == "" {
		return fmt.Sprintf("opencode: HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("opencode: HTTP %d: %s: %s", e.Status, e.Tag, e.Message)
}

// IsUnauthorized reports whether err is a rejection of the password, which
// callers answer by rediscovering it.
func IsUnauthorized(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized
}

// IsNotFound reports whether err says the requested object does not exist.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// newRequest builds an authenticated request. The password goes only into the
// Authorization header, so it never shows up in an error that quotes the URL.
func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	endpoint := c.Endpoint()
	target := strings.TrimRight(endpoint.URL, "/") + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("opencode: build request: %w", err)
	}
	if endpoint.Password != "" {
		req.SetBasicAuth(basicAuthUser, endpoint.Password)
	}
	return req, nil
}

// do sends a JSON request and decodes the response into out, which may be nil.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("opencode: encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := c.newRequest(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opencode: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("opencode: read %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(unwrap(raw), out); err != nil {
		return fmt.Errorf("opencode: decode %s %s: %w", method, path, err)
	}
	return nil
}

// unwrap returns the payload of a `{"data": ...}` envelope. OpenCode wraps most
// responses this way but not all: /api/info and interrupt answer bare.
func unwrap(raw []byte) []byte {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Data == nil {
		return raw
	}
	return envelope.Data
}

func decodeError(status int, raw []byte) error {
	var body struct {
		Tag     string `json:"_tag"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		const limit = 200
		text := string(raw)
		if len(text) > limit {
			text = text[:limit]
		}
		return &APIError{Status: status, Message: text}
	}
	return &APIError{Status: status, Tag: body.Tag, Message: body.Message}
}

// Info describes a running server.
type Info struct {
	Version string   `json:"version"`
	PID     int      `json:"pid"`
	URLs    []string `json:"urls"`
}

// Info returns the server's version and addresses. It is the health check:
// paths such as /health fall through to the web UI and always answer 200.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	err := c.do(ctx, http.MethodGet, "/api/info", nil, nil, &info)
	return info, err
}
