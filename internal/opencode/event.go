package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Event types Talos acts on. They come from captured traffic, not from a
// published schema, and may change between releases; see docs/opencode-api.md.
const (
	EventSessionCreated       = "session.created"
	EventExecutionSucceeded   = "session.execution.succeeded"
	EventExecutionFailed      = "session.execution.failed"
	EventExecutionInterrupted = "session.execution.interrupted"
	EventStepStarted          = "session.step.started"
	EventToolInputStarted     = "session.tool.input.started"
	EventToolFailed           = "session.tool.failed"
	EventUsageUpdated         = "session.usage.updated"
	EventPermissionAsked      = "permission.asked"
	EventPermissionReplied    = "permission.replied"
	EventFormCreated          = "form.created"
)

// ErrorPermissionRejected is the tool error type for a call that a rule or a
// reply denied.
const ErrorPermissionRejected = "permission.rejected"

// Event is one message of the event stream. Data is left raw because its shape
// depends on Type; unknown types are simply passed along.
type Event struct {
	ID      string          `json:"id"`
	Created int64           `json:"created"`
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data"`
}

// EventData is the union of the fields Talos reads from event payloads. Each
// event type sets only some of them.
type EventData struct {
	// SessionID is the session the event belongs to.
	SessionID string `json:"sessionID"`
	// ParentID is set on session.created for a child session.
	ParentID string `json:"parentID"`
	// ID is the permission request ID on permission.asked, the tool call ID on
	// session.tool.* and the form ID on form.cancelled.
	ID string `json:"id"`
	// Name is the tool name on session.tool.input.started.
	Name string `json:"name"`
	// Action and Resources describe a permission.asked request.
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
	// RequestID identifies the request on permission.replied.
	RequestID string `json:"requestID"`
	// Reason says why a turn was interrupted, such as "user" or "shutdown".
	Reason string `json:"reason"`
	// Error is set on failed turns, steps and tool calls.
	Error *ErrorInfo `json:"error"`
	// Cost and Tokens are set on session.usage.updated.
	Cost   float64 `json:"cost"`
	Tokens Tokens  `json:"tokens"`
	// Form is set on form.created.
	Form *Form `json:"form"`
}

// Decode parses the event's payload.
func (e Event) Decode() (EventData, error) {
	var data EventData
	err := json.Unmarshal(e.Data, &data)
	return data, err
}

// StreamOptions tunes Stream. Zero values select the defaults.
type StreamOptions struct {
	// IdleTimeout is how long silence, heartbeats included, may last before the
	// connection counts as dead. Default 60s.
	IdleTimeout time.Duration
	// MinBackoff and MaxBackoff bound the delay between reconnects. Defaults 1s
	// and 30s.
	MinBackoff, MaxBackoff time.Duration
	// OnConnect runs after each successful connection. Events sent during the
	// gap are lost for good, so this is where callers reconcile through REST.
	// It runs on the reader's goroutine and must not block.
	OnConnect func()
	// OnError runs for each failed or ended connection, before the backoff.
	OnError func(error)
}

var errStreamIdle = errors.New("opencode: event stream went silent")

// Stream delivers events to handle until ctx is done, reconnecting with backoff,
// and then returns ctx's error.
//
// handle runs on the reader's goroutine and must not block: OpenCode fails the
// stream of a slow consumer.
func (c *Client) Stream(ctx context.Context, opts StreamOptions, handle func(Event)) error {
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 60 * time.Second
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = time.Second
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 30 * time.Second
	}
	backoff := opts.MinBackoff
	for {
		connected := false
		err := c.readStream(ctx, opts, func() {
			connected = true
			if opts.OnConnect != nil {
				opts.OnConnect()
			}
		}, handle)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if opts.OnError != nil {
			opts.OnError(err)
		}
		// A connection that came up proves the server is back, so the next
		// failure starts over from the short delay.
		if connected {
			backoff = opts.MinBackoff
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, opts.MaxBackoff)
	}
}

// readStream runs one connection until it fails. It always returns an error.
func (c *Client) readStream(ctx context.Context, opts StreamOptions, connected func(), handle func(Event)) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	req, err := c.newRequest(ctx, http.MethodGet, "/api/event", nil, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	// The watchdog cancels the request, which also covers a server that accepts
	// the connection and never sends headers.
	idle := time.AfterFunc(opts.IdleTimeout, func() { cancel(errStreamIdle) })
	defer idle.Stop()

	resp, err := c.http.Do(req)
	if err != nil {
		return streamError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return decodeError(resp.StatusCode, raw)
	}
	connected()

	err = parseSSE(resp.Body, func() { idle.Reset(opts.IdleTimeout) }, handle)
	return streamError(ctx, err)
}

func streamError(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err == nil || errors.Is(err, io.EOF) {
		return errors.New("opencode: event stream closed")
	}
	return fmt.Errorf("opencode: event stream: %w", err)
}

// parseSSE reads Server-Sent Events until r fails, calling alive for every line,
// comments included. A payload that does not decode is skipped: one odd event
// must not cost the connection, and reconciliation covers anything it carried.
func parseSSE(r io.Reader, alive func(), handle func(Event)) error {
	reader := bufio.NewReader(r)
	var data []byte
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return err
		}
		alive()
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0:
			if len(data) > 0 {
				var event Event
				if json.Unmarshal(data, &event) == nil {
					handle(event)
				}
				data = data[:0]
			}
		case bytes.HasPrefix(line, []byte("data:")):
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimPrefix(line[len("data:"):], []byte(" "))...)
		}
	}
}
