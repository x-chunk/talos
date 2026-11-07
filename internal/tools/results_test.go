package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.xchunk.org/argon/argontest"
	"go.xchunk.org/talos/internal/opencode"
)

func TestGetResultTruncatesWithMarker(t *testing.T) {
	f := newFixture(t)
	started := f.start()
	f.backend.mu.Lock()
	f.backend.messages = []opencode.Message{{
		Type:    opencode.MessageAssistant,
		Content: []opencode.ContentBlock{{Type: "text", Text: strings.Repeat("x", 500)}},
	}}
	f.backend.mu.Unlock()
	f.backend.emit(t, opencode.EventToolFailed, map[string]any{
		"sessionID": "ses_1", "error": map[string]any{"type": opencode.ErrorPermissionRejected},
	})
	os.WriteFile(filepath.Join(started.Worktree, "a.txt"), []byte("a\n"), 0o644)

	out, err := argontest.Call[resultOutput](t, f.s, "get_result", map[string]any{"task_id": started.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "[truncated: showing 50 of 500 bytes") {
		t.Errorf("message = %q", out.Message)
	}
	if out.DeniedActions != 1 || !strings.Contains(out.DiffStat, "a.txt") || out.Branch != started.Branch {
		t.Errorf("out = %+v", out)
	}
}

// A large diff must page below Argon's limit, because Argon rejects oversized
// results rather than truncating them.
func TestGetDiffPagesHugePatchThroughOutputLimit(t *testing.T) {
	f := newFixture(t)
	started := f.start()
	huge := strings.Repeat("line of text\n", 200_000)
	os.WriteFile(filepath.Join(started.Worktree, "huge.txt"), []byte(huge), 0o644)

	total, offset, pages := 0, 0, 0
	for {
		page, err := argontest.Call[pageOutput](t, f.s, "get_diff", map[string]any{"task_id": started.TaskID, "offset": offset})
		if err != nil {
			t.Fatal(err)
		}
		total += len(page.Text)
		pages++
		if page.NextOffset == 0 {
			if total != page.TotalBytes {
				t.Errorf("got %d of %d bytes", total, page.TotalBytes)
			}
			break
		}
		offset = page.NextOffset
	}
	if pages < 2 {
		t.Errorf("pages = %d", pages)
	}
}

func TestGetTranscript(t *testing.T) {
	f := newFixture(t)
	started := f.start()
	f.backend.mu.Lock()
	f.backend.messages = []opencode.Message{{Type: opencode.MessageUser, Text: "go"}}
	f.backend.mu.Unlock()
	page, err := argontest.Call[pageOutput](t, f.s, "get_transcript", map[string]any{"task_id": started.TaskID})
	if err != nil || page.Text != "user: go\n" {
		t.Errorf("page = %+v, err = %v", page, err)
	}
}
