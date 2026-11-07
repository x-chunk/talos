package task

import (
	"fmt"
	"time"
)

const (
	watchdogInterval = time.Second
	// interruptGrace is how long a session gets to report its interruption before
	// Talos ends the task anyway. Without it, a wedged session would hold its
	// slot forever.
	interruptGrace = 30 * time.Second
)

func (m *Manager) watchdogLoop() {
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.checkLimits()
		}
	}
}

// checkLimits enforces the wall-clock and permission timeouts. Both interrupt the
// session and keep the worktree.
func (m *Manager) checkLimits() {
	now := m.opts.Now()
	for _, t := range m.List("") {
		if t.State.holdsSlot() {
			m.update(t.ID, func(t *Task) bool { return m.checkTaskLocked(t, now) })
		}
	}
}

func (m *Manager) checkTaskLocked(t *Task, now time.Time) bool {
	if !t.State.holdsSlot() {
		delete(m.interruptSent, t.ID)
		return false
	}
	if t.Intent != IntentNone {
		return m.enforceInterruptLocked(t, now)
	}
	switch {
	case t.Timeout > 0 && now.Sub(t.TurnStartedAt) > t.Timeout:
		t.Error = fmt.Sprintf("timed out after %s", t.Timeout)
		m.interruptSent[t.ID] = now
		m.requestInterruptLocked(t, IntentTimeout)
		return true
	case m.permissionExpired(t, now):
		t.Error = fmt.Sprintf("no answer to a permission request within %s", m.opts.PermissionTimeout)
		m.interruptSent[t.ID] = now
		m.requestInterruptLocked(t, IntentAbort)
		return true
	}
	return false
}

func (m *Manager) permissionExpired(t *Task, now time.Time) bool {
	if m.opts.PermissionTimeout <= 0 {
		return false
	}
	for _, pending := range t.Pending {
		if now.Sub(pending.AskedAt) > m.opts.PermissionTimeout {
			return true
		}
	}
	return false
}

// enforceInterruptLocked ends a task whose interrupt went unanswered. It finishes
// the task by intent, as if the interruption had been reported.
func (m *Manager) enforceInterruptLocked(t *Task, now time.Time) bool {
	sent, ok := m.interruptSent[t.ID]
	if !ok {
		// The intent came from elsewhere, or from before a restart.
		m.interruptSent[t.ID] = now
		return false
	}
	if now.Sub(sent) <= interruptGrace {
		return false
	}
	delete(m.interruptSent, t.ID)
	state := interruptedState(t.Intent, "")
	if state == StateFailed && t.Error == "" {
		t.Error = "the session did not respond to an interrupt"
	}
	m.finishLocked(t, state)
	return true
}
