// Package gitwt performs the git operations behind a task: one linked worktree
// and one branch per task, diffs against the commit the task started from, and
// merging the result back.
//
// It runs the git binary, so behavior matches what the user sees on the command
// line, hooks and config included.
package gitwt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// run executes git in dir and returns its trimmed stdout. Inherited GIT_* path
// variables are dropped because a stray GIT_DIR, set for instance when Talos
// runs from a hook, would redirect every command to another repository.
func run(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	env := []string{"LC_ALL=C"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_DIR=") && !strings.HasPrefix(kv, "GIT_WORK_TREE=") && !strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// Root returns the top level of the checkout containing dir, or an error if dir
// is not inside a git work tree.
func Root(ctx context.Context, dir string) (string, error) {
	return run(ctx, dir, nil, "rev-parse", "--show-toplevel")
}

// Create adds a worktree at path on a new branch and returns the commit it
// started from. An empty baseRef means HEAD. The worktree starts from a commit,
// so uncommitted changes in source are not part of it.
func Create(ctx context.Context, source, path, branch, baseRef string) (string, error) {
	if baseRef == "" {
		baseRef = "HEAD"
	}
	base, err := run(ctx, source, nil, "rev-parse", "--verify", "--end-of-options", baseRef+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve base %q: %w", baseRef, err)
	}
	if _, err := run(ctx, source, nil, "worktree", "add", "-b", branch, path, base); err != nil {
		return "", err
	}
	return base, nil
}

// Remove deletes the worktree and its branch, discarding whatever they hold.
// Callers decide beforehand whether that is acceptable. Parts already gone are
// not an error, so an interrupted cleanup can be retried.
func Remove(ctx context.Context, source, path, branch string) error {
	var errs []error
	if _, statErr := os.Stat(path); statErr == nil {
		if _, err := run(ctx, source, nil, "worktree", "remove", "--force", path); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := run(ctx, source, nil, "worktree", "prune"); err != nil {
		errs = append(errs, err)
	}
	if _, err := run(ctx, source, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		if _, err := run(ctx, source, nil, "branch", "-D", branch); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
