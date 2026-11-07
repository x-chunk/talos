package tools

import (
	"context"

	"go.xchunk.org/argon"
	"go.xchunk.org/talos/internal/task"
)

type cleanupInput struct {
	TaskID        string `json:"task_id" jsonschema:"Task ID from start_task"`
	Merge         bool   `json:"merge,omitempty" jsonschema:"Commit leftover changes and merge the task branch into the source checkout's current branch"`
	CommitMessage string `json:"commit_message,omitempty" jsonschema:"Message for the commit and the merge"`
	Force         bool   `json:"force,omitempty" jsonschema:"Discard unmerged work"`
}

type cleanupOutput struct {
	Committed bool   `json:"committed"`
	Merged    bool   `json:"merged"`
	Target    string `json:"merged_into,omitempty"`
	Removed   bool   `json:"removed"`
}

func registerCleanup(s *argon.Server, m *task.Manager) {
	argon.Tool(s, "cleanup_task",
		"Retire a finished task: remove its worktree and branch, after merging them if merge=true. "+
			"Unmerged or uncommitted work is refused unless merge=true or force=true. "+
			"On a merge conflict the merge is aborted, nothing is removed, and the task can continue through send_message.",
		func(ctx context.Context, in cleanupInput) (cleanupOutput, error) {
			r, err := m.Cleanup(ctx, in.TaskID, task.CleanupRequest{
				Merge:         in.Merge,
				CommitMessage: in.CommitMessage,
				Force:         in.Force,
			})
			if err != nil {
				return cleanupOutput{}, err
			}
			return cleanupOutput{Committed: r.Committed, Merged: r.Merged, Target: r.Target, Removed: true}, nil
		}, argon.Destructive())
}
