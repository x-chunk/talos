package tools

import (
	"context"
	"fmt"
	"time"

	"go.xchunk.org/argon"
	"go.xchunk.org/talos/internal/task"
)

// abortWait gives an interruption time to be reported, so abort_task can usually
// answer with the final state instead of "running".
const abortWait = 5 * time.Second

type messageInput struct {
	TaskID string `json:"task_id" jsonschema:"Task ID from start_task"`
	Text   string `json:"text" jsonschema:"The message for the sub-agent"`
}

type permissionInput struct {
	TaskID    string `json:"task_id" jsonschema:"Task ID from start_task"`
	RequestID string `json:"request_id" jsonschema:"request_id from pending_permissions"`
	Decision  string `json:"decision" jsonschema:"allow or deny"`
	Message   string `json:"message,omitempty" jsonschema:"Explanation passed to the sub-agent"`
}

func registerControl(s *argon.Server, m *task.Manager) {
	argon.Tool(s, "send_message",
		"Send follow-up text to a task's session, keeping its context. On a finished task this starts a new turn, which may queue for a slot. On a running task it steers the current turn. "+
			"This is also how to answer a sub-agent that ended its turn with a question.",
		func(ctx context.Context, in messageInput) (Status, error) {
			t, err := m.SendMessage(ctx, in.TaskID, in.Text)
			if err != nil {
				return Status{}, err
			}
			return statusOf(t), nil
		})

	argon.Tool(s, "respond_permission",
		"Answer a pending permission request reported by wait_task or get_task. allow applies to this one request only.",
		func(ctx context.Context, in permissionInput) (Status, error) {
			if in.Decision != "allow" && in.Decision != "deny" {
				return Status{}, fmt.Errorf("decision %q is not allow or deny", in.Decision)
			}
			t, err := m.RespondPermission(ctx, in.TaskID, in.RequestID, in.Decision == "allow", in.Message)
			if err != nil {
				return Status{}, err
			}
			return statusOf(t), nil
		})

	argon.Tool(s, "abort_task",
		"Stop a task and keep its worktree for review. The state may briefly remain running if the session is slow to stop.",
		func(ctx context.Context, in taskInput) (Status, error) {
			t, err := m.Abort(in.TaskID)
			if err != nil {
				return Status{}, err
			}
			if !t.State.Finished() {
				tasks, err := m.Wait(ctx, []string{in.TaskID}, abortWait)
				if err != nil {
					return Status{}, err
				}
				t = tasks[0]
			}
			return statusOf(t), nil
		}, argon.Idempotent())
}
