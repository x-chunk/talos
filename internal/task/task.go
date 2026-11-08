// Package task is the engine of Talos: the task state machine, the scheduler,
// event handling, watchdogs, reconciliation and persistence.
//
// It knows nothing about MCP. OpenCode and git are reached through small
// interfaces so the logic can be tested with fakes.
package task

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"time"
)

// State is a task's position in its lifecycle.
type State string

// Task states. A finished task can return to StateQueued through a follow-up
// message: only cleanup retires a task for good.
const (
	StateQueued            State = "queued"
	StateRunning           State = "running"
	StateWaitingPermission State = "waiting_permission"
	StateSucceeded         State = "succeeded"
	StateFailed            State = "failed"
	StateAborted           State = "aborted"
	StateTimedOut          State = "timed_out"
)

// Finished reports whether no turn is queued or running.
func (s State) Finished() bool {
	switch s {
	case StateSucceeded, StateFailed, StateAborted, StateTimedOut:
		return true
	}
	return false
}

// holdsSlot reports whether the state counts against max_parallel.
func (s State) holdsSlot() bool {
	return s == StateRunning || s == StateWaitingPermission
}

// canTransition holds the lifecycle's edges. A queued task can fail or abort
// without having run, when its session cannot be created or it is aborted first.
func canTransition(from, to State) bool {
	switch from {
	case StateQueued:
		return to == StateRunning || to == StateFailed || to == StateAborted
	case StateRunning:
		return to == StateWaitingPermission || to.Finished()
	case StateWaitingPermission:
		return to == StateRunning || to.Finished()
	default:
		return to == StateQueued
	}
}

// Intent records why Talos interrupted a session. The interruption's event only
// says "user", so without this a timeout would look like an abort.
type Intent string

// Interrupt intents.
const (
	IntentNone    Intent = ""
	IntentAbort   Intent = "abort"
	IntentTimeout Intent = "timeout"
	IntentFail    Intent = "fail"
)

// PendingPermission is an unanswered request. SessionID is the session that owns
// it, possibly a child, and the only one OpenCode accepts the reply through.
type PendingPermission struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Action    string    `json:"action"`
	Resources []string  `json:"resources"`
	AskedAt   time.Time `json:"asked_at"`
}

// Usage is one session's cumulative cost and token counts.
type Usage struct {
	Cost         float64 `json:"cost"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

// Task is the persisted record of one task. The manager owns live values; callers
// only ever receive copies made by Clone.
type Task struct {
	ID         string `json:"id"`
	State      State  `json:"state"`
	Directory  string `json:"directory"`
	Worktree   string `json:"worktree"`
	Branch     string `json:"branch"`
	BaseCommit string `json:"base_commit"`

	// Prompt is the text the next turn will send. It is cleared once sent.
	Prompt string `json:"prompt,omitempty"`
	Model  string `json:"model,omitempty"`
	Agent  string `json:"agent,omitempty"`

	Timeout  time.Duration `json:"timeout"`
	MaxSteps int           `json:"max_steps"`

	SessionID string `json:"session_id,omitempty"`
	// Children are the sub-sessions spawned by the root session's task tool.
	Children []string `json:"children,omitempty"`

	// Usage holds each session's latest cumulative usage. OpenCode reports totals
	// per session, so the task's figures are sums over this map.
	Usage        map[string]Usage `json:"usage,omitempty"`
	Steps        int              `json:"steps"`
	Cost         float64          `json:"cost"`
	InputTokens  int64            `json:"input_tokens"`
	OutputTokens int64            `json:"output_tokens"`
	// Progress describes what the current turn is doing. It is empty once the
	// task finishes, so a stale tool name never looks like live work.
	Progress      string              `json:"progress,omitempty"`
	Error         string              `json:"error,omitempty"`
	DeniedActions int                 `json:"denied_actions"`
	Pending       []PendingPermission `json:"pending,omitempty"`
	Intent        Intent              `json:"intent,omitempty"`

	// Turn counts prompts started. In-flight work carries the turn it belongs to
	// and gives up if the task has moved on.
	Turn int `json:"turn"`

	// OwnerPID is the Talos process enforcing this task's limits.
	OwnerPID int `json:"owner_pid"`

	CreatedAt time.Time `json:"created_at"`
	// TurnStartedAt anchors the wall-clock timeout, and PromptSentAt tells
	// reconciliation whether an idle message belongs to the current turn.
	TurnStartedAt time.Time `json:"turn_started_at,omitzero"`
	PromptSentAt  time.Time `json:"prompt_sent_at,omitzero"`
	FinishedAt    time.Time `json:"finished_at,omitzero"`
	// Version increases with every change. Wait uses it to detect changes.
	Version uint64 `json:"version"`
}

// Clone returns a copy that shares no mutable state with t.
func (t *Task) Clone() *Task {
	c := *t
	c.Usage = maps.Clone(t.Usage)
	c.Children = append([]string(nil), t.Children...)
	c.Pending = make([]PendingPermission, len(t.Pending))
	for i, p := range t.Pending {
		p.Resources = append([]string(nil), p.Resources...)
		c.Pending[i] = p
	}
	if len(c.Pending) == 0 {
		c.Pending = nil
	}
	return &c
}

// ownsSession reports whether id is the root session or one of its children.
func (t *Task) ownsSession(id string) bool {
	if id == t.SessionID {
		return id != ""
	}
	for _, child := range t.Children {
		if child == id {
			return true
		}
	}
	return false
}

// transition moves t to state, stamping or clearing FinishedAt. An illegal edge
// returns an error and leaves t unchanged, so a late event cannot, for example,
// turn an aborted task into a succeeded one.
func (t *Task) transition(to State, now time.Time) error {
	if !canTransition(t.State, to) {
		return fmt.Errorf("task %s: illegal transition %s -> %s", t.ID, t.State, to)
	}
	t.State = to
	if to.Finished() {
		t.FinishedAt = now
		t.Progress = ""
		t.Pending = nil
		t.Intent = IntentNone
	} else {
		t.FinishedAt = time.Time{}
	}
	return nil
}

// newID returns a short random task ID. It ends up in branch names and paths, so
// it is restricted to hex.
func newID() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate task id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
