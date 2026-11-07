package tools

import (
	"os"
	"path/filepath"
	"testing"

	"go.xchunk.org/argon/argontest"
	"go.xchunk.org/talos/internal/opencode"
)

func TestCleanupTool(t *testing.T) {
	f := newFixture(t)
	started := f.start()
	os.WriteFile(filepath.Join(started.Worktree, "new.txt"), []byte("n\n"), 0o644)
	f.backend.emit(t, opencode.EventExecutionSucceeded, map[string]any{"sessionID": "ses_1"})

	if _, err := argontest.Call[cleanupOutput](t, f.s, "cleanup_task", map[string]any{"task_id": started.TaskID}); err == nil {
		t.Fatal("unmerged work must be refused")
	}
	out, err := argontest.Call[cleanupOutput](t, f.s, "cleanup_task", map[string]any{
		"task_id": started.TaskID, "merge": true, "commit_message": "feat: add new",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Merged || !out.Removed || out.Target != "main" {
		t.Errorf("out = %+v", out)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "new.txt")); err != nil {
		t.Error(err)
	}
}
