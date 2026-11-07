package task

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"go.xchunk.org/talos/internal/opencode"
)

func assistant(blocks ...opencode.ContentBlock) opencode.Message {
	return opencode.Message{Type: opencode.MessageAssistant, Content: blocks}
}

func TestResultFindsFinalTextExcludingReasoning(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.ResultLimit = 100 })
	task := h.live()
	h.backend.set(func(f *fakeBackend) {
		f.messages = map[string][]opencode.Message{task.SessionID: {
			{Type: opencode.MessageIdle},
			assistant(opencode.ContentBlock{Type: "tool", Name: "write"}),
			assistant(opencode.ContentBlock{Type: "reasoning", Text: "secret thoughts"}, opencode.ContentBlock{Type: "text", Text: "final answer"}),
			assistant(opencode.ContentBlock{Type: "text", Text: "older"}),
		}}
	})
	if err := os.WriteFile(filepath.Join(task.Worktree, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := h.m.Result(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Message != "final answer" {
		t.Errorf("message = %q", result.Message)
	}
	if !strings.Contains(result.DiffStat, "new.txt") {
		t.Errorf("stat = %q", result.DiffStat)
	}
}

func TestResultSurvivesServerFailure(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	h.backend.set(func(f *fakeBackend) { f.messagesErr = errors.New("down") })
	result, err := h.m.Result(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.MessageError != "down" {
		t.Errorf("result = %+v", result)
	}
}

func TestTruncateKeepsValidUTF8AndMarks(t *testing.T) {
	text := strings.Repeat("я", 100)
	got := truncate(text, 51)
	if !utf8.ValidString(got) || !strings.Contains(got, "[truncated: showing 50 of 200 bytes") {
		t.Errorf("got = %q", got)
	}
	if truncate("short", 100) != "short" {
		t.Error("short text must be unchanged")
	}
}

func TestPaginateCoversEverythingAndRespectsRunes(t *testing.T) {
	text := strings.Repeat("aя", 1000)
	var rebuilt strings.Builder
	offset := 0
	for {
		page, err := paginate(text, offset, 101)
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.ValidString(page.Text) || len(page.Text) > 101 {
			t.Fatalf("page = %q", page.Text)
		}
		rebuilt.WriteString(page.Text)
		if page.NextOffset == 0 {
			break
		}
		offset = page.NextOffset
	}
	if rebuilt.String() != text {
		t.Error("pages do not rebuild the text")
	}
	if _, err := paginate("x", 5, 10); err == nil {
		t.Error("want an error for a bad offset")
	}
}

func TestDiffAndTranscript(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	os.WriteFile(filepath.Join(task.Worktree, "new.txt"), []byte("x\n"), 0o644)
	page, err := h.m.Diff(t.Context(), task.ID, "", 0)
	if err != nil || !strings.Contains(page.Text, "+x") {
		t.Errorf("page = %+v, err = %v", page, err)
	}

	h.backend.set(func(f *fakeBackend) {
		f.messages = map[string][]opencode.Message{task.SessionID: {
			{Type: opencode.MessageIdle, Outcome: "succeeded"},
			assistant(opencode.ContentBlock{Type: "reasoning", Text: "hidden"}, opencode.ContentBlock{Type: "tool", Name: "write"}, opencode.ContentBlock{Type: "text", Text: "done"}),
			{Type: opencode.MessageUser, Text: "go"},
		}}
	})
	page, err = h.m.Transcript(t.Context(), task.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "user: go\ntool: write\nassistant: done\n[turn succeeded]\n"
	if page.Text != want {
		t.Errorf("transcript = %q", page.Text)
	}
}

func TestTranscriptMarksOmittedHistory(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	h.backend.set(func(f *fakeBackend) {
		f.messages = map[string][]opencode.Message{task.SessionID: make([]opencode.Message, transcriptMessageLimit)}
	})
	page, err := h.m.Transcript(t.Context(), task.ID, 0)
	if err != nil || !strings.HasPrefix(page.Text, "[older items omitted") {
		t.Errorf("page = %+v, err = %v", page, err)
	}
}
