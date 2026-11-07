package task

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.xchunk.org/talos/internal/opencode"
)

// SendMessage delivers follow-up text. A finished task re-enters through the
// scheduler as a new turn in the same session. On a running task the text steers
// the current turn. It fails while a turn is still being set up, because the text
// could be neither merged into a prompt already on its way nor steered into a
// session that may not exist yet.
func (m *Manager) SendMessage(ctx context.Context, id, text string) (*Task, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("text is empty")
	}
	var steerSession string
	var opErr error
	m.update(id, func(t *Task) bool {
		switch {
		case m.cleaning[id]:
			opErr = errors.New("the task is being cleaned up")
			return false
		case t.State == StateQueued:
			t.Prompt += "\n\n" + text
			return true
		case t.State.Finished():
			t.Prompt = text
			if err := t.transition(StateQueued, m.opts.Now()); err != nil {
				opErr = err
				return false
			}
			return true
		case t.PromptSentAt.IsZero() || t.Prompt != "":
			opErr = errors.New("the turn is still starting; try again shortly")
			return false
		}
		steerSession = t.SessionID
		return false
	})
	if opErr != nil {
		return nil, opErr
	}
	if steerSession != "" {
		if _, err := m.opts.Backend.Prompt(ctx, steerSession, text, opencode.DeliverySteer); err != nil {
			return nil, fmt.Errorf("send message: %w", err)
		}
		// Whatever idle item exists now may predate this message, so the turn is
		// not over until a newer one appears.
		m.update(id, func(t *Task) bool {
			if t.SessionID != steerSession || !t.State.holdsSlot() {
				return false
			}
			t.PromptSentAt = m.opts.Now()
			return true
		})
	}
	return m.Get(id)
}

// RespondPermission answers a pending request, replying through the session that
// owns it. The event that follows may arrive at any time, or never, so the pending
// entry is removed here; a repeat is harmless.
func (m *Manager) RespondPermission(ctx context.Context, id, requestID string, allow bool, message string) (*Task, error) {
	t, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	sessionID := ""
	for _, pending := range t.Pending {
		if pending.ID == requestID {
			sessionID = pending.SessionID
		}
	}
	if sessionID == "" {
		return nil, fmt.Errorf("task %s has no pending request %s", id, requestID)
	}
	decision := opencode.DecisionReject
	if allow {
		decision = opencode.DecisionOnce
	}
	if err := m.opts.Backend.ReplyPermission(ctx, sessionID, requestID, decision, message); err != nil {
		return nil, fmt.Errorf("reply to permission: %w", err)
	}
	m.update(id, func(t *Task) bool { return m.removePendingLocked(t, requestID) })
	return m.Get(id)
}

// Abort stops a task and keeps its worktree. A task with no turn underway ends at
// once. Otherwise the session is interrupted and the task ends when that is
// reported, so the returned state may still be running. Aborting a finished task
// changes nothing.
func (m *Manager) Abort(id string) (*Task, error) {
	m.update(id, func(t *Task) bool {
		switch {
		case t.State.Finished():
			return false
		case t.State == StateQueued || t.PromptSentAt.IsZero():
			// startTurn notices the finished state and gives up.
			m.finishLocked(t, StateAborted)
			return true
		}
		m.interruptSent[t.ID] = m.opts.Now()
		m.requestInterruptLocked(t, IntentAbort)
		return true
	})
	return m.Get(id)
}
