package gitwt

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ConflictError reports a merge that stopped on conflicts and was aborted. The
// source checkout is back as it was, and the task branch is untouched.
type ConflictError struct {
	Files []string
}

func (e *ConflictError) Error() string {
	return "merge conflict in: " + strings.Join(e.Files, ", ")
}

// Dirty reports whether the worktree has uncommitted changes or untracked files.
func Dirty(ctx context.Context, path string) (bool, error) {
	out, err := run(ctx, path, nil, "status", "--porcelain")
	return out != "", err
}

// Merged reports whether branch is fully contained in source's HEAD.
func Merged(ctx context.Context, source, branch string) (bool, error) {
	_, err := run(ctx, source, nil, "merge-base", "--is-ancestor", branch, "HEAD")
	if err == nil {
		return true, nil
	}
	// Exit status 1 is the "no" answer; anything else, such as a missing branch,
	// is a real failure and exits with 128.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// Commit stages everything in the worktree and commits it, reporting whether
// there was anything to commit. If git has no identity configured, Talos signs
// the commit itself rather than fail at the last step of a finished task.
func Commit(ctx context.Context, path, message string) (bool, error) {
	dirty, err := Dirty(ctx, path)
	if err != nil || !dirty {
		return false, err
	}
	if _, err := run(ctx, path, nil, "add", "-A"); err != nil {
		return false, err
	}
	// Status also reports files whose only change vanishes once staged.
	if _, err := run(ctx, path, nil, "diff", "--cached", "--quiet"); err == nil {
		return false, nil
	}
	args := []string{"commit", "-q", "-m", message}
	if _, err := run(ctx, path, nil, "var", "GIT_COMMITTER_IDENT"); err != nil {
		args = append([]string{"-c", "user.name=Talos", "-c", "user.email=talos@localhost"}, args...)
	}
	if _, err := run(ctx, path, nil, args...); err != nil {
		return false, err
	}
	return true, nil
}

// Merge merges branch into whatever source has checked out, always creating a
// merge commit, and returns the checked-out branch's name.
//
// On conflict it aborts and returns a *ConflictError. It refuses to start while
// source is already mid-merge, because the abort would destroy that merge. Other
// refusals by git, such as local changes the merge would overwrite, leave source
// untouched and come back as plain errors.
func Merge(ctx context.Context, source, branch, message string) (string, error) {
	if _, err := run(ctx, source, nil, "rev-parse", "--verify", "--quiet", "MERGE_HEAD"); err == nil {
		return "", errors.New("source checkout is already in the middle of a merge")
	}
	target, err := run(ctx, source, nil, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	if target == "" {
		return "", errors.New("source checkout has a detached HEAD")
	}
	args := []string{"merge", "--no-ff", "--no-edit", "-m", message, branch}
	if _, err := run(ctx, source, nil, "var", "GIT_COMMITTER_IDENT"); err != nil {
		args = append([]string{"-c", "user.name=Talos", "-c", "user.email=talos@localhost"}, args...)
	}
	_, mergeErr := run(ctx, source, nil, args...)
	if mergeErr == nil {
		return target, nil
	}
	if _, err := run(ctx, source, nil, "rev-parse", "--verify", "--quiet", "MERGE_HEAD"); err != nil {
		return "", mergeErr
	}
	files, listErr := run(ctx, source, nil, "diff", "--name-only", "--diff-filter=U")
	if _, err := run(ctx, source, nil, "merge", "--abort"); err != nil {
		return "", fmt.Errorf("abort failed merge: %w", err)
	}
	if listErr != nil {
		return "", listErr
	}
	return "", &ConflictError{Files: strings.Split(files, "\n")}
}
