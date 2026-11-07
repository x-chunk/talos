package task

import (
	"errors"
	"syscall"
)

// Adopt loads the tasks this instance is responsible for: its own and those whose
// owner died. Call it before serving requests, or a task that exists on disk would
// look unknown to a caller. Failures are logged, because one unreadable record
// should not keep the server from starting.
//
// Tasks of a live instance stay untouched, so two instances never both enforce
// limits on one task. A reused PID can make a dead owner look alive, which only
// delays adoption until that process exits and Talos starts again.
func (m *Manager) Adopt() {
	tasks, err := m.opts.Store.Load()
	if err != nil {
		m.opts.Logger.Warn("load tasks", "error", err)
	}
	for _, loaded := range tasks {
		if !m.ownerGone(loaded.OwnerPID) {
			continue
		}
		claimed, err := m.opts.Store.Claim(loaded.ID, func(current *Task) bool {
			return m.ownerGone(current.OwnerPID)
		}, m.opts.PID)
		if err != nil {
			m.opts.Logger.Warn("claim task", "task", loaded.ID, "error", err)
			continue
		}
		if claimed == nil {
			continue
		}
		requeueIfNeverStarted(claimed)
		m.mu.Lock()
		m.tasks[claimed.ID] = claimed
		m.touchLocked(claimed)
		m.mu.Unlock()
	}
}

// requeueIfNeverStarted sends a task back to the queue if its previous owner died
// before the prompt went out. Nothing ran, so it can safely start over. This edge
// is deliberately outside the state machine: it undoes a promotion instead of
// describing something that happened to the task.
func requeueIfNeverStarted(t *Task) {
	if t.State == StateRunning && t.PromptSentAt.IsZero() {
		t.State = StateQueued
		t.Turn--
	}
}

// ownerGone reports whether pid no longer runs, or is this very process, which
// can only mean an earlier instance had the same PID.
func (m *Manager) ownerGone(pid int) bool {
	if pid == m.opts.PID {
		return true
	}
	return !processAlive(pid)
}

// processAlive probes with signal 0. EPERM means the process exists but belongs
// to someone else, which still counts as alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
