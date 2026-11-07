package tools

import (
	"context"

	"go.xchunk.org/argon"
	"go.xchunk.org/talos/internal/task"
)

type resultOutput struct {
	Status
	Message      string `json:"message"`
	MessageError string `json:"message_error,omitempty"`
	DiffStat     string `json:"diff_stat"`
	Branch       string `json:"branch"`
	Worktree     string `json:"worktree"`
	// DeniedActions matters because a turn can end in "succeeded" even though a
	// permission was rejected: the model simply carried on without it.
	DeniedActions int   `json:"denied_actions"`
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
}

type diffInput struct {
	TaskID string `json:"task_id" jsonschema:"Task ID from start_task"`
	Path   string `json:"path,omitempty" jsonschema:"Limit the patch to this file, relative to the repository root"`
	Offset int    `json:"offset,omitempty" jsonschema:"Byte offset of the page, from next_offset"`
}

type transcriptInput struct {
	TaskID string `json:"task_id" jsonschema:"Task ID from start_task"`
	Offset int    `json:"offset,omitempty" jsonschema:"Byte offset of the page, from next_offset"`
}

type pageOutput struct {
	Text string `json:"text"`
	// NextOffset is zero on the last page.
	NextOffset int `json:"next_offset"`
	TotalBytes int `json:"total_bytes"`
}

func pageOf(p task.Page) pageOutput {
	return pageOutput{Text: p.Text, NextOffset: p.NextOffset, TotalBytes: p.Total}
}

func registerResults(s *argon.Server, m *task.Manager) {
	argon.Tool(s, "get_result",
		"Return a task's final message (truncated with a marker), diff stat, branch, worktree, usage and denied_actions. "+
			"\"succeeded\" only means the turn ended cleanly: check denied_actions and the diff to see whether the work got done.",
		func(ctx context.Context, in taskInput) (resultOutput, error) {
			r, err := m.Result(ctx, in.TaskID)
			if err != nil {
				return resultOutput{}, err
			}
			return resultOutput{
				Status:        statusOf(r.Task),
				Message:       r.Message,
				MessageError:  r.MessageError,
				DiffStat:      r.DiffStat,
				Branch:        r.Task.Branch,
				Worktree:      r.Task.Worktree,
				DeniedActions: r.Task.DeniedActions,
				InputTokens:   r.Task.InputTokens,
				OutputTokens:  r.Task.OutputTokens,
			}, nil
		}, argon.ReadOnly())

	argon.Tool(s, "get_diff",
		"Return one page of the task's patch against its base commit, covering commits, edits and untracked files. Pass next_offset as offset until it is 0.",
		func(ctx context.Context, in diffInput) (pageOutput, error) {
			page, err := m.Diff(ctx, in.TaskID, in.Path, in.Offset)
			if err != nil {
				return pageOutput{}, err
			}
			return pageOf(page), nil
		}, argon.ReadOnly())

	argon.Tool(s, "get_transcript",
		"Return one page of a compact transcript: user and assistant text, tool names and turn outcomes. Use only when the final message is not enough. Pass next_offset as offset until it is 0.",
		func(ctx context.Context, in transcriptInput) (pageOutput, error) {
			page, err := m.Transcript(ctx, in.TaskID, in.Offset)
			if err != nil {
				return pageOutput{}, err
			}
			return pageOf(page), nil
		}, argon.ReadOnly())
}
