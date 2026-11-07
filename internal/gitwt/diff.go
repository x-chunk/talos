package gitwt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// snapshot stages everything in the worktree into a throwaway index and calls fn
// with the environment that selects it. Staging is what brings untracked files
// into the diff, and the separate index keeps the real one untouched while a
// sub-agent may be working. Ignored files stay out, as they would in a commit.
func snapshot(ctx context.Context, path string, fn func(env []string) error) error {
	dir, err := os.MkdirTemp("", "talos-index-")
	if err != nil {
		return fmt.Errorf("create temporary index: %w", err)
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	// Seeding from HEAD lets add reuse existing entries instead of hashing the
	// whole tree.
	if _, err := run(ctx, path, env, "read-tree", "HEAD"); err != nil {
		return err
	}
	if _, err := run(ctx, path, env, "add", "-A"); err != nil {
		return err
	}
	return fn(env)
}

// Diff returns the patch of everything the task changed since base: agent
// commits, uncommitted edits and untracked files. A non-empty file limits it to
// that path.
func Diff(ctx context.Context, path, base, file string) (string, error) {
	args := []string{"diff", "--cached", "--no-ext-diff", base}
	if file != "" {
		args = append(args, "--", file)
	}
	return diffWith(ctx, path, args)
}

// DiffStat returns the `git diff --stat` summary of the same change as Diff, or
// an empty string if nothing changed.
func DiffStat(ctx context.Context, path, base string) (string, error) {
	return diffWith(ctx, path, []string{"diff", "--cached", "--stat", base})
}

func diffWith(ctx context.Context, path string, args []string) (string, error) {
	var out string
	err := snapshot(ctx, path, func(env []string) error {
		var err error
		out, err = run(ctx, path, env, args...)
		return err
	})
	return out, err
}
