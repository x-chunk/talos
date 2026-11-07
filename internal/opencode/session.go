package opencode

import (
	"context"
	"net/http"
	"net/url"
)

// ModelRef names a model the way OpenCode does.
type ModelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

// Location is where a session works.
type Location struct {
	Directory string `json:"directory"`
}

// Permission rule effects.
const (
	EffectAllow = "allow"
	EffectDeny  = "deny"
	EffectAsk   = "ask"
)

// PermissionRule is one entry of a session's ruleset. A later rule overrides an
// earlier one, and child sessions inherit the set.
type PermissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// CreateSessionRequest is the body of POST /api/session. OpenCode validates
// neither Location nor Model here: a bad model surfaces later as a failed turn.
type CreateSessionRequest struct {
	Title       string           `json:"title,omitempty"`
	Agent       string           `json:"agent,omitempty"`
	Model       *ModelRef        `json:"model,omitempty"`
	Location    *Location        `json:"location,omitempty"`
	Permissions []PermissionRule `json:"permissions,omitempty"`
}

// Tokens is token usage as OpenCode reports it.
type Tokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
}

// Session is the subset of a session's info that Talos reads. Outcome and
// Time.Idle are cleared while a turn runs.
type Session struct {
	ID       string  `json:"id"`
	ParentID string  `json:"parentID"`
	Cost     float64 `json:"cost"`
	Tokens   Tokens  `json:"tokens"`
	Outcome  string  `json:"outcome"`
	Time     struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
		Idle    int64 `json:"idle"`
	} `json:"time"`
}

// CreateSession creates a session without starting a turn.
func (c *Client) CreateSession(ctx context.Context, req CreateSessionRequest) (Session, error) {
	var session Session
	err := c.do(ctx, http.MethodPost, "/api/session", nil, req, &session)
	return session, err
}

// GetSession returns a session. The error satisfies IsNotFound if it is gone.
func (c *Client) GetSession(ctx context.Context, sessionID string) (Session, error) {
	var session Session
	err := c.do(ctx, http.MethodGet, sessionPath(sessionID), nil, nil, &session)
	return session, err
}

// ChildSessions returns the sessions spawned directly by parentID. It recovers
// children whose session.created event was lost.
func (c *Client) ChildSessions(ctx context.Context, parentID string) ([]Session, error) {
	var sessions []Session
	err := c.do(ctx, http.MethodGet, "/api/session", url.Values{"parentID": {parentID}}, nil, &sessions)
	return sessions, err
}

// ActiveSessions returns the IDs of sessions with a running turn. A session
// waiting on a permission is still listed.
func (c *Client) ActiveSessions(ctx context.Context) (map[string]bool, error) {
	var raw map[string]struct {
		Type string `json:"type"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/session/active", nil, nil, &raw); err != nil {
		return nil, err
	}
	active := make(map[string]bool, len(raw))
	for id := range raw {
		active[id] = true
	}
	return active, nil
}

// Prompt delivery modes.
const (
	// DeliverySteer injects the text into a running turn.
	DeliverySteer = "steer"
	// DeliveryQueue holds the text until the current turn ends.
	DeliveryQueue = "queue"
)

// Prompt sends text to a session and returns the new message's ID. It returns
// as soon as the message is accepted; the turn's result arrives as events.
func (c *Client) Prompt(ctx context.Context, sessionID, text, delivery string) (string, error) {
	in := struct {
		Text     string `json:"text"`
		Delivery string `json:"delivery,omitempty"`
	}{text, delivery}
	var out struct {
		ID string `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, sessionPath(sessionID)+"/prompt", nil, in, &out)
	return out.ID, err
}

// Interrupt stops a session's turn and reports whether one was running.
// Interrupting an idle session is a harmless false.
func (c *Client) Interrupt(ctx context.Context, sessionID string) (bool, error) {
	var out struct {
		Interrupted bool `json:"interrupted"`
	}
	err := c.do(ctx, http.MethodPost, sessionPath(sessionID)+"/interrupt", nil, nil, &out)
	return out.Interrupted, err
}

// PermissionRequest is a pending request that blocks a turn.
type PermissionRequest struct {
	ID        string   `json:"id"`
	SessionID string   `json:"sessionID"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
}

// Permissions lists one session's pending requests. It uses the per-session
// route because the global one returned nothing while a request was pending.
func (c *Client) Permissions(ctx context.Context, sessionID string) ([]PermissionRequest, error) {
	var requests []PermissionRequest
	err := c.do(ctx, http.MethodGet, sessionPath(sessionID)+"/permission", nil, nil, &requests)
	return requests, err
}

// Permission reply decisions.
const (
	DecisionOnce   = "once"
	DecisionAlways = "always"
	DecisionReject = "reject"
)

// ReplyPermission answers a request. sessionID must be the session that owns
// the request, which may be a child of the task's root session.
func (c *Client) ReplyPermission(ctx context.Context, sessionID, requestID, decision, message string) error {
	in := struct {
		Decision string `json:"decision"`
		Message  string `json:"message,omitempty"`
	}{decision, message}
	path := sessionPath(sessionID) + "/permission/" + url.PathEscape(requestID) + "/reply"
	return c.do(ctx, http.MethodPost, path, nil, in, nil)
}

func sessionPath(sessionID string) string {
	return "/api/session/" + url.PathEscape(sessionID)
}
