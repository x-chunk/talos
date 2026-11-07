package gitwt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repo creates a repository with one commit and returns its path. Identity
// comes from the environment so tests do not depend on the user's git config.
func repo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "a.txt", "one\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "init")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := run(context.Background(), dir, nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// worktree creates a task worktree on branch talos/x.
func worktree(t *testing.T, source string) (path, base string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "wt")
	base, err := Create(t.Context(), source, path, "talos/x", "")
	if err != nil {
		t.Fatal(err)
	}
	return path, base
}

func TestRoot(t *testing.T) {
	dir := repo(t)
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	root, err := Root(t.Context(), sub)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if root != want {
		t.Errorf("root = %q, want %q", root, want)
	}
	if _, err := Root(t.Context(), t.TempDir()); err == nil {
		t.Error("want an error outside a repository")
	}
}

func TestCreateStartsFromCommitWithoutDirtyChanges(t *testing.T) {
	source := repo(t)
	write(t, source, "a.txt", "dirty\n")
	path, base := worktree(t, source)
	if base != git(t, source, "rev-parse", "HEAD") {
		t.Errorf("base = %s", base)
	}
	got, _ := os.ReadFile(filepath.Join(path, "a.txt"))
	if string(got) != "one\n" {
		t.Errorf("a.txt = %q", got)
	}
	if b := git(t, path, "branch", "--show-current"); b != "talos/x" {
		t.Errorf("branch = %q", b)
	}
}

func TestCreateWithBaseRef(t *testing.T) {
	source := repo(t)
	first := git(t, source, "rev-parse", "HEAD")
	git(t, source, "commit", "-q", "--allow-empty", "-m", "second")
	base, err := Create(t.Context(), source, filepath.Join(t.TempDir(), "wt"), "talos/y", first)
	if err != nil || base != first {
		t.Errorf("base = %s, err = %v", base, err)
	}
}

func TestCreateRejectsBadRef(t *testing.T) {
	source := repo(t)
	for _, ref := range []string{"nope", "--help"} {
		_, err := Create(t.Context(), source, filepath.Join(t.TempDir(), "wt"), "talos/y", ref)
		if err == nil {
			t.Errorf("ref %q: want an error", ref)
		}
	}
}

func TestRemoveIsThoroughAndRetryable(t *testing.T) {
	source := repo(t)
	path, _ := worktree(t, source)
	write(t, path, "untracked.txt", "x")
	for range 2 {
		if err := Remove(t.Context(), source, path, "talos/x"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("worktree still exists: %v", err)
	}
	if out := git(t, source, "branch", "--list", "talos/x"); out != "" {
		t.Errorf("branch remains: %q", out)
	}
	if out := git(t, source, "worktree", "list"); strings.Count(out, "\n") != 0 {
		t.Errorf("worktrees = %q", out)
	}
}
