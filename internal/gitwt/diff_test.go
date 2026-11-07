package gitwt

import (
	"strings"
	"testing"
)

func TestDiffCoversCommitsEditsAndUntrackedFiles(t *testing.T) {
	source := repo(t)
	path, base := worktree(t, source)

	write(t, path, "committed.txt", "c\n")
	git(t, path, "add", "committed.txt")
	git(t, path, "commit", "-qm", "agent commit")
	write(t, path, "a.txt", "two\n")
	write(t, path, "untracked.txt", "u\n")
	write(t, path, ".gitignore", "ignored.txt\n")
	write(t, path, "ignored.txt", "i\n")

	diff, err := Diff(t.Context(), path, base, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"committed.txt", "+two", "untracked.txt"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff lacks %q:\n%s", want, diff)
		}
	}
	if strings.Contains(diff, "ignored.txt\n+") || strings.Contains(diff, "b/ignored.txt") {
		t.Errorf("diff includes an ignored file:\n%s", diff)
	}

	stat, err := DiffStat(t.Context(), path, base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stat, "untracked.txt") || !strings.Contains(stat, "changed") {
		t.Errorf("stat = %q", stat)
	}
}

func TestDiffLimitedToFile(t *testing.T) {
	source := repo(t)
	path, base := worktree(t, source)
	write(t, path, "a.txt", "two\n")
	write(t, path, "b.txt", "b\n")
	diff, err := Diff(t.Context(), path, base, "b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "b.txt") || strings.Contains(diff, "a.txt") {
		t.Errorf("diff = %s", diff)
	}
}

func TestDiffLeavesRealIndexAlone(t *testing.T) {
	source := repo(t)
	path, base := worktree(t, source)
	write(t, path, "new.txt", "n\n")
	if _, err := Diff(t.Context(), path, base, ""); err != nil {
		t.Fatal(err)
	}
	if status := git(t, path, "status", "--porcelain"); status != "?? new.txt" {
		t.Errorf("status = %q", status)
	}
}

func TestDiffEmptyWhenNothingChanged(t *testing.T) {
	source := repo(t)
	path, base := worktree(t, source)
	stat, err := DiffStat(t.Context(), path, base)
	if err != nil || stat != "" {
		t.Errorf("stat = %q, err = %v", stat, err)
	}
}
