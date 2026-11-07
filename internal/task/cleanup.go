package task

import (
	"context"
	"errors"
	"fmt"

	"go.xchunk.org/talos/internal/gitwt"
)

// CleanupRequest says how to retire a task.
type CleanupRequest struct {
	// Merge commits leftover changes and merges the branch into whatever the
	// source checkout has checked out. Talos merges only when asked.
	Merge bool
	// CommitMessage is used for both commits. Default "talos: task <id>".
	CommitMessage string
	// Force discards unmerged work. Merge takes precedence: a failed merge stops
	// the cleanup even with Force.
	Force bool
}

// CleanupResult says what Cleanup did.
type CleanupResult struct {
	Committed bool
	Merged    bool
	// Target is the branch merged into, when Merged is true.
	Target string
}

// Cleanup retires a finished task, removing its worktree and branch. It refuses to
// lose work: a task with uncommitted or unmerged changes needs Merge or Force.
// If the merge conflicts, it returns a *gitwt.ConflictError, removes nothing and
// the task stays available. Any commit made before that remains on the task's
// branch.
func (m *Manager) Cleanup(ctx context.Context, id string, req CleanupRequest) (*CleanupResult, error) {
	t, err := m.beginCleanup(id)
	if err != nil {
		return nil, err
	}
	defer m.endCleanup(id)

	result := &CleanupResult{}
	message := req.CommitMessage
	if message == "" {
		message = "talos: task " + id
	}
	if req.Merge {
		if result.Committed, err = gitwt.Commit(ctx, t.Worktree, message); err != nil {
			return nil, fmt.Errorf("commit task changes: %w", err)
		}
		merged, err := gitwt.Merged(ctx, t.Directory, t.Branch)
		if err != nil {
			return nil, err
		}
		if !merged {
			if result.Target, err = gitwt.Merge(ctx, t.Directory, t.Branch, message); err != nil {
				return nil, err
			}
			result.Merged = true
		}
	} else if !req.Force {
		if err := refuseLosingWork(ctx, t); err != nil {
			return nil, err
		}
	}

	if err := gitwt.Remove(ctx, t.Directory, t.Worktree, t.Branch); err != nil {
		return nil, fmt.Errorf("remove worktree (any merge already happened): %w", err)
	}
	if err := m.opts.Store.Delete(id); err != nil {
		m.opts.Logger.Warn("delete task record", "task", id, "error", err)
	}
	m.mu.Lock()
	delete(m.tasks, id)
	delete(m.dirty, id)
	delete(m.interruptSent, id)
	m.notifyLocked()
	m.mu.Unlock()
	return result, nil
}

func refuseLosingWork(ctx context.Context, t *Task) error {
	dirty, err := gitwt.Dirty(ctx, t.Worktree)
	if err != nil {
		return err
	}
	if dirty {
		return errors.New("the worktree has uncommitted changes; pass merge=true to keep them or force=true to discard them")
	}
	merged, err := gitwt.Merged(ctx, t.Directory, t.Branch)
	if err != nil {
		return err
	}
	if !merged {
		return fmt.Errorf("branch %s has unmerged commits; pass merge=true to keep them or force=true to discard them", t.Branch)
	}
	return nil
}

// beginCleanup claims a finished task, so a concurrent send_message cannot restart
// it while its worktree is going away.
func (m *Manager) beginCleanup(id string) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if !t.State.Finished() {
		return nil, fmt.Errorf("task %s is %s; abort it first", id, t.State)
	}
	if m.cleaning[id] {
		return nil, fmt.Errorf("task %s is already being cleaned up", id)
	}
	m.cleaning[id] = true
	return t.Clone(), nil
}

func (m *Manager) endCleanup(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.cleaning, id)
}
