package gitwt

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitStagesEverything(t *testing.T) {
	source := repo(t)
	path, _ := worktree(t, source)
	if committed, err := Commit(t.Context(), path, "m"); err != nil || committed {
		t.Fatalf("clean: committed = %v, err = %v", committed, err)
	}
	write(t, path, "new.txt", "n\n")
	committed, err := Commit(t.Context(), path, "add new")
	if err != nil || !committed {
		t.Fatalf("committed = %v, err = %v", committed, err)
	}
	if dirty, _ := Dirty(t.Context(), path); dirty {
		t.Error("worktree still dirty")
	}
	if msg := git(t, path, "log", "-1", "--format=%s"); msg != "add new" {
		t.Errorf("message = %q", msg)
	}
}

func TestMergeCreatesMergeCommit(t *testing.T) {
	source := repo(t)
	path, _ := worktree(t, source)
	write(t, path, "new.txt", "n\n")
	Commit(t.Context(), path, "add new")

	if merged, err := Merged(t.Context(), source, "talos/x"); err != nil || merged {
		t.Fatalf("before: merged = %v, err = %v", merged, err)
	}
	target, err := Merge(t.Context(), source, "talos/x", "merge task")
	if err != nil {
		t.Fatal(err)
	}
	if target != "main" {
		t.Errorf("target = %q", target)
	}
	if parents := git(t, source, "log", "-1", "--format=%P"); len(strings.Fields(parents)) != 2 {
		t.Errorf("parents = %q", parents)
	}
	if merged, err := Merged(t.Context(), source, "talos/x"); err != nil || !merged {
		t.Errorf("after: merged = %v, err = %v", merged, err)
	}
	if _, err := os.Stat(filepath.Join(source, "new.txt")); err != nil {
		t.Error(err)
	}
}

func conflicting(t *testing.T) (source, head string) {
	t.Helper()
	source = repo(t)
	path, _ := worktree(t, source)
	write(t, path, "a.txt", "theirs\n")
	Commit(t.Context(), path, "theirs")
	write(t, source, "a.txt", "ours\n")
	git(t, source, "commit", "-qam", "ours")
	return source, git(t, source, "rev-parse", "HEAD")
}

func TestMergeConflictAbortsAndKeepsEverything(t *testing.T) {
	source, head := conflicting(t)
	_, err := Merge(t.Context(), source, "talos/x", "m")
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v", err)
	}
	if len(conflict.Files) != 1 || conflict.Files[0] != "a.txt" {
		t.Errorf("files = %v", conflict.Files)
	}
	if got := git(t, source, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved to %s", got)
	}
	if status := git(t, source, "status", "--porcelain"); status != "" {
		t.Errorf("status = %q", status)
	}
	if out := git(t, source, "branch", "--list", "talos/x"); out == "" {
		t.Error("task branch is gone")
	}
}

func TestMergeRefusedByDirtyCheckoutLeavesItAlone(t *testing.T) {
	source := repo(t)
	path, _ := worktree(t, source)
	write(t, path, "a.txt", "theirs\n")
	Commit(t.Context(), path, "theirs")
	write(t, source, "a.txt", "uncommitted\n")

	_, err := Merge(t.Context(), source, "talos/x", "m")
	var conflict *ConflictError
	if err == nil || errors.As(err, &conflict) {
		t.Fatalf("err = %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(source, "a.txt"))
	if string(got) != "uncommitted\n" {
		t.Errorf("a.txt = %q", got)
	}
}

func TestMergeDoesNotAbortSomeoneElsesMerge(t *testing.T) {
	source, _ := conflicting(t)
	if _, err := run(t.Context(), source, nil, "merge", "talos/x"); err == nil {
		t.Fatal("want the user's merge to conflict")
	}
	if _, err := Merge(t.Context(), source, "talos/x", "m"); err == nil {
		t.Fatal("want a refusal")
	}
	git(t, source, "rev-parse", "--verify", "MERGE_HEAD")
}

func TestMergeRefusesDetachedHead(t *testing.T) {
	source := repo(t)
	worktree(t, source)
	git(t, source, "checkout", "-q", "--detach")
	if _, err := Merge(t.Context(), source, "talos/x", "m"); err == nil {
		t.Fatal("want a refusal")
	}
}

func TestMergedMissingBranchIsAnError(t *testing.T) {
	if _, err := Merged(t.Context(), repo(t), "nope"); err == nil {
		t.Fatal("want an error")
	}
}
