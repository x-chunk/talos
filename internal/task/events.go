package task

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.xchunk.org/talos/internal/opencode"
)

// interruptTimeout bounds the interrupt call itself, not the session's reaction.
const interruptTimeout = 15 * time.Second

// connect makes the server reachable and, the first time, starts following its
// events. It warns once if the server's major version is untested.
func (m *Manager) connect(ctx context.Context) error {
	info, err := m.opts.Backend.Ensure(ctx)
	if err != nil {
		return err
	}
	m.versionOnce.Do(func() {
		if !strings.HasPrefix(info.Version, "2.") {
			m.opts.Logger.Warn("untested OpenCode major version", "version", info.Version, "tested", "2.0.32")
		}
	})
	m.streamOnce.Do(func() { m.wg.Go(m.streamLoop) })
	return nil
}

func (m *Manager) streamLoop() {
	opts := opencode.StreamOptions{
		OnConnect: m.requestReconcile,
		OnError:   func(err error) { m.opts.Logger.Info("event stream interrupted", "error", err) },
	}
	_ = m.opts.Backend.Stream(m.ctx, opts, m.handleEvent)
}

// handleEvent applies one event. It runs on the stream reader and only touches
// memory; anything needing the network goes to its own goroutine.
func (m *Manager) handleEvent(event opencode.Event) {
	switch event.Type {
	case opencode.EventSessionCreated,
		opencode.EventExecutionSucceeded,
		opencode.EventExecutionFailed,
		opencode.EventExecutionInterrupted,
		opencode.EventStepStarted,
		opencode.EventToolInputStarted,
		opencode.EventToolFailed,
		opencode.EventUsageUpdated,
		opencode.EventPermissionAsked,
		opencode.EventPermissionReplied,
		opencode.EventFormCreated:
	default:
		// High-volume deltas and unknown events end here, before any decoding.
		return
	}
	data, err := event.Decode()
	if err != nil {
		m.opts.Logger.Debug("undecodable event", "type", event.Type, "error", err)
		return
	}
	if event.Type == opencode.EventFormCreated && data.Form != nil {
		data.SessionID = data.Form.SessionID
	}
	lookup := data.SessionID
	if event.Type == opencode.EventSessionCreated {
		lookup = data.ParentID
	}
	id, ok := m.taskForSession(lookup)
	if !ok {
		return
	}
	m.update(id, func(t *Task) bool { return m.applyEventLocked(t, event, data) })
}

func (m *Manager) taskForSession(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, t := range m.tasks {
		if t.ownsSession(sessionID) {
			return id, true
		}
	}
	return "", false
}

// applyEventLocked changes t according to the event and reports whether it did.
func (m *Manager) applyEventLocked(t *Task, event opencode.Event, data opencode.EventData) bool {
	root := data.SessionID == t.SessionID
	switch event.Type {
	case opencode.EventSessionCreated:
		if t.ownsSession(data.SessionID) {
			return false
		}
		t.Children = append(t.Children, data.SessionID)
		return true

	case opencode.EventPermissionAsked:
		return m.addPendingLocked(t, opencode.PermissionRequest{
			ID: data.ID, SessionID: data.SessionID, Action: data.Action, Resources: data.Resources,
		})

	case opencode.EventPermissionReplied:
		return m.removePendingLocked(t, data.RequestID)

	case opencode.EventStepStarted:
		if t.State.Finished() {
			return false
		}
		t.Steps++
		if t.Steps > t.MaxSteps && t.Intent == IntentNone {
			t.Error = fmt.Sprintf("step limit of %d exceeded", t.MaxSteps)
			m.requestInterruptLocked(t, IntentFail)
		}
		return true

	case opencode.EventToolInputStarted:
		if t.State.Finished() {
			return false
		}
		t.Progress = "running tool " + data.Name
		return true

	case opencode.EventToolFailed:
		if data.Error == nil || data.Error.Type != opencode.ErrorPermissionRejected {
			return false
		}
		t.DeniedActions++
		return true

	case opencode.EventUsageUpdated:
		if t.Usage == nil {
			t.Usage = make(map[string]Usage)
		}
		t.Usage[data.SessionID] = Usage{Cost: data.Cost, InputTokens: data.Tokens.Input, OutputTokens: data.Tokens.Output}
		t.Cost, t.InputTokens, t.OutputTokens = 0, 0, 0
		for _, usage := range t.Usage {
			t.Cost += usage.Cost
			t.InputTokens += usage.InputTokens
			t.OutputTokens += usage.OutputTokens
		}
		return true

	case opencode.EventFormCreated:
		// Question tools are denied by the session's rules, so this is only a
		// safety net. A form would block the turn with nobody there to answer it.
		if t.State.Finished() || t.Intent != IntentNone {
			return false
		}
		t.Error = "the sub-agent opened a form, which Talos cannot answer"
		t.Intent = IntentFail
		sessionID, formID := data.SessionID, data.Form.ID
		m.wg.Go(func() {
			ctx, cancel := context.WithTimeout(m.ctx, interruptTimeout)
			defer cancel()
			if err := m.opts.Backend.CancelForm(ctx, sessionID, formID); err != nil {
				m.opts.Logger.Warn("cancel form", "form", formID, "error", err)
			}
		})
		return true

	case opencode.EventExecutionSucceeded:
		return root && m.applyOutcomeLocked(t, opencode.OutcomeSucceeded, nil, "")
	case opencode.EventExecutionFailed:
		return root && m.applyOutcomeLocked(t, opencode.OutcomeFailed, data.Error, "")
	case opencode.EventExecutionInterrupted:
		return root && m.applyOutcomeLocked(t, opencode.OutcomeInterrupted, nil, data.Reason)
	}
	return false
}

func (m *Manager) addPendingLocked(t *Task, req opencode.PermissionRequest) bool {
	if !t.State.holdsSlot() {
		return false
	}
	for _, pending := range t.Pending {
		if pending.ID == req.ID {
			return false
		}
	}
	t.Pending = append(t.Pending, PendingPermission{
		ID: req.ID, SessionID: req.SessionID, Action: req.Action, Resources: req.Resources, AskedAt: m.opts.Now(),
	})
	if t.State == StateRunning {
		_ = t.transition(StateWaitingPermission, m.opts.Now())
	}
	return true
}

func (m *Manager) removePendingLocked(t *Task, requestID string) bool {
	for i, pending := range t.Pending {
		if pending.ID != requestID {
			continue
		}
		t.Pending = append(t.Pending[:i], t.Pending[i+1:]...)
		if len(t.Pending) == 0 && t.State == StateWaitingPermission {
			_ = t.transition(StateRunning, m.opts.Now())
		}
		return true
	}
	return false
}

// applyOutcomeLocked ends the current turn. It is shared by the event path and
// reconciliation, which learn the same facts from different places. An outcome
// is ignored until the prompt has gone out, because it can only belong to an
// earlier turn.
func (m *Manager) applyOutcomeLocked(t *Task, outcome string, errInfo *opencode.ErrorInfo, reason string) bool {
	if !t.State.holdsSlot() || t.PromptSentAt.IsZero() {
		return false
	}
	switch outcome {
	case opencode.OutcomeSucceeded:
		// This also wins over a pending interrupt: the work completed before the
		// interrupt could stop it.
		t.Error = ""
		m.finishLocked(t, StateSucceeded)
	case opencode.OutcomeFailed:
		if errInfo != nil {
			t.Error = errInfo.Message
			if errInfo.Type != "" {
				t.Error = errInfo.Type + ": " + errInfo.Message
			}
		} else if t.Error == "" {
			t.Error = "the turn failed"
		}
		m.finishLocked(t, StateFailed)
	case opencode.OutcomeInterrupted:
		state := interruptedState(t.Intent, reason)
		if state == StateFailed && t.Error == "" {
			t.Error = "the session was interrupted: " + reason
		}
		m.finishLocked(t, state)
	default:
		return false
	}
	return true
}

// interruptedState maps an interruption to a final state. Talos's recorded intent
// decides, since the event's reason cannot tell a timeout from an abort. Without
// one, "user" means someone stopped it in OpenCode directly.
func interruptedState(intent Intent, reason string) State {
	switch intent {
	case IntentAbort:
		return StateAborted
	case IntentTimeout:
		return StateTimedOut
	case IntentFail:
		return StateFailed
	}
	if reason == "user" {
		return StateAborted
	}
	return StateFailed
}

// requestInterruptLocked records why the session is being stopped and sends the
// interrupt from another goroutine. The task finishes when the interruption is
// reported, not here, so a turn that completes first keeps its true result.
func (m *Manager) requestInterruptLocked(t *Task, intent Intent) {
	t.Intent = intent
	sessionID := t.SessionID
	if sessionID == "" {
		return
	}
	m.wg.Go(func() {
		ctx, cancel := context.WithTimeout(m.ctx, interruptTimeout)
		defer cancel()
		if _, err := m.opts.Backend.Interrupt(ctx, sessionID); err != nil {
			m.opts.Logger.Warn("interrupt session", "session", sessionID, "error", err)
		}
		// The interruption's event can be lost like any other.
		m.requestReconcile()
	})
}
