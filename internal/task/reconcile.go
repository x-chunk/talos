package task

import (
	"context"
	"time"

	"go.xchunk.org/talos/internal/opencode"
)

const (
	// reconcileInterval is the safety net: events can be lost at any time, not
	// only across a reconnect.
	reconcileInterval = 30 * time.Second
	// reconcileGrace is how long a session may be neither active nor idle after
	// its prompt went out, since a prompt can still be on its way.
	reconcileGrace   = 15 * time.Second
	reconcileTimeout = 30 * time.Second
	// recentMessageLimit only needs to reach the newest timeline item.
	recentMessageLimit = 1
)

func (m *Manager) requestReconcile() {
	select {
	case m.reconcileNow <- struct{}{}:
	default:
	}
}

func (m *Manager) reconcileLoop() {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
		case <-m.reconcileNow:
		}
		ctx, cancel := context.WithTimeout(m.ctx, reconcileTimeout)
		m.reconcile(ctx)
		cancel()
	}
}

// reconcile brings live tasks in line with the server through REST, which is the
// source of truth: events are volatile and undocumented. A failed request leaves
// the task as it is until the next pass, because a server that is merely
// unreachable says nothing about the session.
func (m *Manager) reconcile(ctx context.Context) {
	var live []*Task
	for _, t := range m.List("") {
		if t.State.holdsSlot() && !t.PromptSentAt.IsZero() {
			live = append(live, t)
		}
	}
	if len(live) == 0 {
		return
	}
	if err := m.connect(ctx); err != nil {
		m.opts.Logger.Info("reconcile: server unavailable", "error", err)
		return
	}
	active, err := m.opts.Backend.ActiveSessions(ctx)
	if err != nil {
		m.opts.Logger.Info("reconcile: list active sessions", "error", err)
		return
	}
	for _, t := range live {
		if active[t.SessionID] {
			m.refreshPermissions(ctx, t)
		} else {
			m.settleInactive(ctx, t)
		}
	}
}

// refreshPermissions rebuilds the pending list of a running session and its
// descendants. A session stays active while waiting on a permission, so this is
// the only way to notice a request whose event was lost.
func (m *Manager) refreshPermissions(ctx context.Context, snapshot *Task) {
	sessions := []string{snapshot.SessionID}
	known := map[string]bool{snapshot.SessionID: true}
	var requests []opencode.PermissionRequest
	for i := 0; i < len(sessions); i++ {
		id := sessions[i]
		pending, err := m.opts.Backend.Permissions(ctx, id)
		if err != nil {
			m.opts.Logger.Info("reconcile: list permissions", "session", id, "error", err)
			return
		}
		requests = append(requests, pending...)
		children, err := m.opts.Backend.ChildSessions(ctx, id)
		if err != nil {
			m.opts.Logger.Info("reconcile: list children", "session", id, "error", err)
			return
		}
		for _, child := range children {
			if !known[child.ID] {
				known[child.ID] = true
				sessions = append(sessions, child.ID)
			}
		}
	}
	m.update(snapshot.ID, func(t *Task) bool {
		if t.Turn != snapshot.Turn || !t.State.holdsSlot() {
			return false
		}
		changed := false
		for _, id := range sessions[1:] {
			if !t.ownsSession(id) {
				t.Children = append(t.Children, id)
				changed = true
			}
		}
		present := make(map[string]bool, len(requests))
		for _, req := range requests {
			present[req.ID] = true
			if m.addPendingLocked(t, req) {
				changed = true
			}
		}
		for _, pending := range append([]PendingPermission(nil), t.Pending...) {
			if !present[pending.ID] && m.removePendingLocked(t, pending.ID) {
				changed = true
			}
		}
		return changed
	})
}

// settleInactive handles a session that is no longer running: the newest timeline
// item says how its turn ended, including a turn that ended while nobody watched.
func (m *Manager) settleInactive(ctx context.Context, snapshot *Task) {
	messages, err := m.opts.Backend.RecentMessages(ctx, snapshot.SessionID, recentMessageLimit)
	lost := opencode.IsNotFound(err)
	if err != nil && !lost {
		m.opts.Logger.Info("reconcile: read messages", "session", snapshot.SessionID, "error", err)
		return
	}
	var idle *opencode.Message
	if len(messages) > 0 && messages[0].Type == opencode.MessageIdle &&
		messages[0].Time.Created >= snapshot.PromptSentAt.UnixMilli() {
		idle = &messages[0]
	}
	m.update(snapshot.ID, func(t *Task) bool {
		// The turn may have moved on while the request was in flight.
		if t.Turn != snapshot.Turn || !t.PromptSentAt.Equal(snapshot.PromptSentAt) || !t.State.holdsSlot() {
			return false
		}
		if idle != nil {
			// An idle item carries no interruption reason, so without an intent a
			// missed shutdown reads as an abort rather than a failure.
			return m.applyOutcomeLocked(t, idle.Outcome, idle.Error, "user")
		}
		if !lost && m.opts.Now().Sub(t.PromptSentAt) < reconcileGrace {
			return false
		}
		// Talos was already stopping this session, so its absence is the expected
		// result and the recorded intent still decides.
		if t.Intent != IntentNone {
			m.finishLocked(t, interruptedState(t.Intent, ""))
			return true
		}
		t.Error = "session lost"
		m.finishLocked(t, StateFailed)
		return true
	})
}
