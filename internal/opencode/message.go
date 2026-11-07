package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
)

// Message timeline item types Talos reads.
const (
	MessageUser      = "user"
	MessageAssistant = "assistant"
	MessageIdle      = "idle"
)

// Turn outcomes, as carried by an idle message and by Session.Outcome.
const (
	OutcomeSucceeded   = "succeeded"
	OutcomeFailed      = "failed"
	OutcomeInterrupted = "interrupted"
)

// ContentBlock is one block of an assistant message. Reasoning blocks must be
// left out of anything presented as the final answer.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

// ErrorInfo is the error of a failed turn, step or tool call.
type ErrorInfo struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Message is one item of a session's timeline. Items of other types decode with
// only ID and Type set.
type Message struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Text    string         `json:"text"`
	Content []ContentBlock `json:"content"`
	Outcome string         `json:"outcome"`
	Error   *ErrorInfo     `json:"-"`
	Time    struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

// UnmarshalJSON tolerates a non-object error, since the field is undocumented
// beyond the type and message seen in captured traffic.
func (m *Message) UnmarshalJSON(raw []byte) error {
	type plain Message
	aux := struct {
		*plain
		Error json.RawMessage `json:"error"`
	}{plain: (*plain)(m)}
	if err := json.Unmarshal(raw, &aux); err != nil {
		return err
	}
	if len(aux.Error) == 0 {
		return nil
	}
	var info ErrorInfo
	if err := json.Unmarshal(aux.Error, &info); err != nil {
		info = ErrorInfo{Message: string(aux.Error)}
	}
	if info != (ErrorInfo{}) {
		m.Error = &info
	}
	return nil
}

// RecentMessages returns up to limit of a session's newest timeline items,
// newest first.
func (c *Client) RecentMessages(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	query := url.Values{"order": {"desc"}, "limit": {strconv.Itoa(limit)}}
	var messages []Message
	err := c.do(ctx, http.MethodGet, sessionPath(sessionID)+"/message", query, nil, &messages)
	return messages, err
}

// Form is a pending question, created by OpenCode's question tool. It blocks
// the turn until it is answered or cancelled.
type Form struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
}

// Forms lists a session's pending forms.
func (c *Client) Forms(ctx context.Context, sessionID string) ([]Form, error) {
	var forms []Form
	err := c.do(ctx, http.MethodGet, sessionPath(sessionID)+"/form", nil, nil, &forms)
	return forms, err
}

// CancelForm cancels a form, which interrupts the turn with reason "shutdown".
func (c *Client) CancelForm(ctx context.Context, sessionID, formID string) error {
	return c.do(ctx, http.MethodDelete, sessionPath(sessionID)+"/form/"+url.PathEscape(formID), nil, nil, nil)
}
