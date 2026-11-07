package opencode

import (
	"context"
	"errors"
	"sync"
)

// Connection is a Client that finds its server on demand. Nothing happens until
// the first need, so an MCP client can initialize while OpenCode is down.
//
// It is safe for concurrent use.
type Connection struct {
	*Client
	locator   Locator
	autostart bool

	mu sync.Mutex
}

// NewConnection returns a connection that uses locator. With autostart, Ensure
// starts the managed service if it is not reachable.
func NewConnection(locator Locator, autostart bool) *Connection {
	client := NewClient(Endpoint{}, nil)
	client.refresh = locator.Find
	return &Connection{Client: client, locator: locator, autostart: autostart}
}

// Status describes the server as of now, without starting anything. Err is the
// reason it is not reachable.
type Status struct {
	Reachable bool
	URL       string
	Version   string
	Source    string
	Err       error
}

// Status checks the server without starting it.
func (c *Connection) Status(ctx context.Context) Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := Status{Source: c.locator.Source()}
	info, err := c.probe(ctx)
	status.URL = c.Endpoint().URL
	if err != nil {
		status.Err = err
		return status
	}
	status.Reachable, status.Version = true, info.Version
	return status
}

// Ensure makes the server reachable, starting the service if autostart allows.
func (c *Connection) Ensure(ctx context.Context) (Info, error) {
	return c.ensure(ctx, c.autostart)
}

// Start is Ensure for an explicit request: it starts the service even when
// autostart is off.
func (c *Connection) Start(ctx context.Context) (Info, error) {
	return c.ensure(ctx, true)
}

func (c *Connection) ensure(ctx context.Context, start bool) (Info, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, err := c.probe(ctx)
	if err == nil {
		return info, nil
	}
	if !start || c.locator.Source() == SourceExplicit {
		return Info{}, err
	}
	endpoint, err := c.locator.Start(ctx)
	if err != nil {
		return Info{}, err
	}
	c.SetEndpoint(endpoint)
	return c.Info(ctx)
}

// probe re-reads the locator on every call, since the service may have moved to
// a new port since the last one.
func (c *Connection) probe(ctx context.Context) (Info, error) {
	endpoint, err := c.locator.Find()
	if err != nil {
		if errors.Is(err, ErrServiceNotRunning) {
			c.SetEndpoint(Endpoint{})
		}
		return Info{}, err
	}
	c.SetEndpoint(endpoint)
	return c.Info(ctx)
}
