package task

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.xchunk.org/talos/internal/gitwt"
	"go.xchunk.org/talos/internal/opencode"
)

// finished returns a succeeded task, with changes made by fn in its worktree.
func (h *harness) finished(fn func(worktree string)) *Task {
	h.t.Helper()
	task := h.live()
	if fn != nil {
		fn(task.Worktree)
	}
	h.m.handleEvent(event(opencode.EventExecutionSucceeded, map[string]any{"sessionID": task.SessionID}))
	return h.waitState(task.ID, StateSucceeded)
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (h *harness) assertGone(task *Task) {
	h.t.Helper()
	if _, err := os.Stat(task.Worktree); !os.IsNotExist(err) {
		h.t.Errorf("worktree remains: %v", err)
	}
	if out := runGit(h.t, h.repo, "branch", "--list", task.Branch); out != "" {
		h.t.Errorf("branch remains: %s", out)
	}
	if _, err := h.m.Get(task.ID); !errors.Is(err, ErrNotFound) {
		h.t.Errorf("task remains: %v", err)
	}
	if _, err := os.Stat(h.store.path(task.ID)); !os.IsNotExist(err) {
		h.t.Errorf("record remains: %v", err)
	}
}

func TestCleanupMergeBringsWorkHome(t *testing.T) {
	h := newHarness(t)
	task := h.finished(func(wt string) { writeFile(t, wt, "new.txt", "n\n") })
	result, err := h.m.Cleanup(t.Context(), task.ID, CleanupRequest{Merge: true, CommitMessage: "add new"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Committed || !result.Merged || result.Target != "main" {
		t.Errorf("result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(h.repo, "new.txt")); err != nil {
		t.Error(err)
	}
	if parents := runGit(t, h.repo, "log", "-1", "--format=%P"); len(strings.Fields(parents)) != 2 {
		t.Errorf("parents = %q", parents)
	}
	h.assertGone(task)
}

func TestCleanupWithoutChangesNeedsNoFlags(t *testing.T) {
	h := newHarness(t)
	task := h.finished(nil)
	result, err := h.m.Cleanup(t.Context(), task.ID, CleanupRequest{})
	if err != nil || result.Merged {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	h.assertGone(task)
}

func TestCleanupRefusesToLoseWork(t *testing.T) {
	tests := []struct {
		name string
		work func(t *testing.T, wt string)
		want string
	}{
		{"uncommitted", func(t *testing.T, wt string) { writeFile(t, wt, "new.txt", "n\n") }, "uncommitted"},
		{"unmerged commit", func(t *testing.T, wt string) {
			writeFile(t, wt, "new.txt", "n\n")
			runGit(t, wt, "add", "-A")
			runGit(t, wt, "commit", "-qm", "agent")
		}, "unmerged"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			task := h.finished(func(wt string) { tt.work(t, wt) })
			_, err := h.m.Cleanup(t.Context(), task.ID, CleanupRequest{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v", err)
			}
			if _, err := os.Stat(filepath.Join(task.Worktree, "new.txt")); err != nil {
				t.Error("the work was lost")
			}
			if _, err := h.m.Cleanup(t.Context(), task.ID, CleanupRequest{Force: true}); err != nil {
				t.Fatal(err)
			}
			h.assertGone(task)
		})
	}
}

func TestCleanupConflictRemovesNothing(t *testing.T) {
	h := newHarness(t)
	writeFile(t, h.repo, "a.txt", "base\n")
	runGit(t, h.repo, "add", "-A")
	runGit(t, h.repo, "commit", "-qm", "base")
	task := h.finished(func(wt string) { writeFile(t, wt, "a.txt", "theirs\n") })
	writeFile(t, h.repo, "a.txt", "ours\n")
	runGit(t, h.repo, "commit", "-qam", "ours")

	_, err := h.m.Cleanup(t.Context(), task.ID, CleanupRequest{Merge: true, Force: true})
	var conflict *gitwt.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(task.Worktree); err != nil {
		t.Error("the worktree was removed")
	}
	if got := h.get(task.ID); got.State != StateSucceeded {
		t.Errorf("state = %s", got.State)
	}
	if status := runGit(t, h.repo, "status", "--porcelain"); status != "" {
		t.Errorf("source is dirty: %q", status)
	}
	// The task can continue: nothing is stuck in "cleaning".
	if _, err := h.m.SendMessage(t.Context(), task.ID, "rebase please"); err != nil {
		t.Error(err)
	}
}

func TestCleanupRefusesLiveTask(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	if _, err := h.m.Cleanup(t.Context(), task.ID, CleanupRequest{Force: true}); err == nil || !strings.Contains(err.Error(), "abort") {
		t.Errorf("err = %v", err)
	}
}
