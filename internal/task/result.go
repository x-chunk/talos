package task

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.xchunk.org/talos/internal/gitwt"
	"go.xchunk.org/talos/internal/opencode"
)

const (
	// resultMessageLimit is how far back to look for the final answer. Tool-only
	// messages can follow the last text block.
	resultMessageLimit = 50
	// transcriptMessageLimit caps what a transcript covers. OpenCode pages by
	// cursor, and a transcript this long is already past what a reader can use.
	transcriptMessageLimit = 500
)

// Result is what the orchestrator needs to judge a finished task.
type Result struct {
	Task *Task
	// Message is the last assistant text, cut to the result limit with a marker.
	// Reasoning is excluded.
	Message string
	// MessageError says why Message is missing, for example because the server is
	// down. The diff comes from git and stays available either way.
	MessageError string
	DiffStat     string
}

// Result gathers a task's final message and diff stat. A running task's result
// reflects its progress so far.
func (m *Manager) Result(ctx context.Context, id string) (*Result, error) {
	t, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	stat, err := gitwt.DiffStat(ctx, t.Worktree, t.BaseCommit)
	if err != nil {
		return nil, fmt.Errorf("diff stat: %w", err)
	}
	result := &Result{Task: t, DiffStat: stat}
	if t.SessionID == "" {
		return result, nil
	}
	messages, err := m.opts.Backend.RecentMessages(ctx, t.SessionID, resultMessageLimit)
	if err != nil {
		result.MessageError = err.Error()
		return result, nil
	}
	result.Message = truncate(finalText(messages), m.opts.ResultLimit)
	return result, nil
}

// finalText returns the text of the newest assistant message that has any.
// messages are ordered newest first.
func finalText(messages []opencode.Message) string {
	for _, message := range messages {
		if message.Type != opencode.MessageAssistant {
			continue
		}
		var parts []string
		for _, block := range message.Content {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

func truncate(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	cut := runeBoundary(text, limit)
	return fmt.Sprintf("%s\n[truncated: showing %d of %d bytes; use get_transcript for more]", text[:cut], cut, len(text))
}

// runeBoundary moves offset back to the start of a UTF-8 sequence, so a cut never
// produces invalid text.
func runeBoundary(text string, offset int) int {
	for offset > 0 && offset < len(text) && !utf8.RuneStart(text[offset]) {
		offset--
	}
	return offset
}

// Page is one slice of a long text. NextOffset is zero on the last page.
type Page struct {
	Text       string
	NextOffset int
	Total      int
}

// paginate exists because Argon fails a result over its output limit instead of
// truncating it, so long outputs have to stay below the limit themselves.
func paginate(text string, offset, size int) (Page, error) {
	if offset < 0 || offset > len(text) {
		return Page{}, fmt.Errorf("offset %d is outside the text of %d bytes", offset, len(text))
	}
	end := offset + size
	if end >= len(text) {
		return Page{Text: text[offset:], Total: len(text)}, nil
	}
	end = runeBoundary(text, end)
	if end <= offset {
		end = offset + size
	}
	return Page{Text: text[offset:end], NextOffset: end, Total: len(text)}, nil
}

// PageSize is the size of a page of diff or transcript. It leaves a wide margin
// below Argon's default limit of 1 MiB, which also has to fit the JSON encoding.
const PageSize = 128 << 10

// Diff returns a page of the task's patch, optionally limited to one file.
func (m *Manager) Diff(ctx context.Context, id, file string, offset int) (Page, error) {
	t, err := m.Get(id)
	if err != nil {
		return Page{}, err
	}
	patch, err := gitwt.Diff(ctx, t.Worktree, t.BaseCommit, file)
	if err != nil {
		return Page{}, err
	}
	return paginate(patch, offset, PageSize)
}

// Transcript returns a page of a compact transcript: user text, assistant text,
// tool names and turn outcomes, without reasoning or tool output.
func (m *Manager) Transcript(ctx context.Context, id string, offset int) (Page, error) {
	t, err := m.Get(id)
	if err != nil {
		return Page{}, err
	}
	if t.SessionID == "" {
		return paginate("", offset, PageSize)
	}
	messages, err := m.opts.Backend.RecentMessages(ctx, t.SessionID, transcriptMessageLimit)
	if err != nil {
		return Page{}, err
	}
	return paginate(formatTranscript(messages), offset, PageSize)
}

func formatTranscript(newestFirst []opencode.Message) string {
	messages := slices.Clone(newestFirst)
	slices.Reverse(messages)
	var b strings.Builder
	for _, message := range messages {
		switch message.Type {
		case opencode.MessageUser:
			fmt.Fprintf(&b, "user: %s\n", message.Text)
		case opencode.MessageAssistant:
			for _, block := range message.Content {
				switch block.Type {
				case "text":
					fmt.Fprintf(&b, "assistant: %s\n", block.Text)
				case "tool":
					fmt.Fprintf(&b, "tool: %s\n", block.Name)
				}
			}
		case opencode.MessageIdle:
			fmt.Fprintf(&b, "[turn %s]\n", message.Outcome)
		}
	}
	return b.String()
}
