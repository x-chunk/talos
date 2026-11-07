// Package tools exposes the task engine as MCP tools. Handlers stay thin: they
// translate between tool schemas and task.Manager, whose contract they inherit.
package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.xchunk.org/argon"
	"go.xchunk.org/talos/internal/task"
)

const (
	defaultWait = 30 * time.Second
	maxWait     = 300 * time.Second
	// waitToolTimeout sits above maxWait because Argon's 60s default would cancel
	// a long wait.
	waitToolTimeout = maxWait + 10*time.Second
	// startToolTimeout allows for creating a worktree of a large repository.
	startToolTimeout = 5 * time.Minute
)

// Register adds the task tools to s.
func Register(s *argon.Server, m *task.Manager) {
	registerLifecycle(s, m)
}

// Pending is a permission request awaiting respond_permission.
type Pending struct {
	RequestID string   `json:"request_id"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
}

// Status is the compact view of a task. It deliberately has no transcript, since
// keeping the orchestrator's context small is the point of Talos.
type Status struct {
	TaskID         string    `json:"task_id"`
	State          string    `json:"state"`
	ElapsedSeconds int64     `json:"elapsed_seconds"`
	Steps          int       `json:"steps"`
	CostUSD        float64   `json:"cost_usd"`
	Progress       string    `json:"progress,omitempty"`
	Error          string    `json:"error,omitempty"`
	Pending        []Pending `json:"pending_permissions,omitempty"`
}

func statusOf(t *task.Task) Status {
	s := Status{
		TaskID:   t.ID,
		State:    string(t.State),
		Steps:    t.Steps,
		CostUSD:  t.Cost,
		Progress: t.Progress,
		Error:    t.Error,
	}
	switch {
	case t.TurnStartedAt.IsZero():
	case !t.FinishedAt.IsZero():
		s.ElapsedSeconds = int64(t.FinishedAt.Sub(t.TurnStartedAt).Seconds())
	default:
		s.ElapsedSeconds = int64(time.Since(t.TurnStartedAt).Seconds())
	}
	for _, p := range t.Pending {
		s.Pending = append(s.Pending, Pending{RequestID: p.ID, Action: p.Action, Resources: p.Resources})
	}
	return s
}

type startInput struct {
	Directory      string `json:"directory" jsonschema:"Absolute path inside a git repository under an allowed root"`
	Prompt         string `json:"prompt" jsonschema:"What the sub-agent should do"`
	Model          string `json:"model,omitempty" jsonschema:"provider/model. Defaults to TALOS_DEFAULT_MODEL, then to OpenCode's own default"`
	Agent          string `json:"agent,omitempty" jsonschema:"OpenCode agent name"`
	BaseRef        string `json:"base_ref,omitempty" jsonschema:"Commit or branch to start from. Defaults to HEAD"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Wall-clock limit per turn"`
	MaxSteps       int    `json:"max_steps,omitempty" jsonschema:"Model step limit per turn"`
}

type startOutput struct {
	TaskID   string `json:"task_id"`
	State    string `json:"state"`
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"`
}

type taskInput struct {
	TaskID string `json:"task_id" jsonschema:"Task ID from start_task"`
}

type waitInput struct {
	TaskIDs        []string `json:"task_ids" jsonschema:"Tasks to watch. Drop finished IDs from later calls, or the call returns at once"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"How long to block. Default 30, maximum 300"`
}

type waitOutput struct {
	Tasks []Status `json:"tasks"`
}

type listInput struct {
	State string `json:"state,omitempty" jsonschema:"Only tasks in this state"`
}

type listOutput struct {
	Tasks []string `json:"tasks"`
}

func registerLifecycle(s *argon.Server, m *task.Manager) {
	argon.Tool(s, "start_task",
		"Start an OpenCode sub-agent on its own branch and git worktree, and return its ID at once. "+
			"The worktree starts from a commit, so uncommitted changes in the source checkout are not visible to the sub-agent: commit first if it needs them. "+
			"Follow up with wait_task, then get_result.",
		func(ctx context.Context, in startInput) (startOutput, error) {
			t, err := m.Start(ctx, task.StartRequest{
				Directory: in.Directory,
				Prompt:    in.Prompt,
				Model:     in.Model,
				Agent:     in.Agent,
				BaseRef:   in.BaseRef,
				Timeout:   time.Duration(in.TimeoutSeconds) * time.Second,
				MaxSteps:  in.MaxSteps,
			})
			if err != nil {
				return startOutput{}, err
			}
			return startOutput{TaskID: t.ID, State: string(t.State), Branch: t.Branch, Worktree: t.Worktree}, nil
		}, argon.ToolTimeout(startToolTimeout))

	argon.Tool(s, "get_task",
		"Return one task's state, elapsed time, step count, cost and a short progress line. Never includes the transcript.",
		func(_ context.Context, in taskInput) (Status, error) {
			t, err := m.Get(in.TaskID)
			if err != nil {
				return Status{}, err
			}
			return statusOf(t), nil
		}, argon.ReadOnly())

	argon.Tool(s, "wait_task",
		"Block until any listed task is finished or waiting on a permission, or until the timeout, and return all listed tasks. "+
			"Pending permissions are included inline; answer them with respond_permission. Preferred over polling get_task.",
		func(ctx context.Context, in waitInput) (waitOutput, error) {
			timeout := time.Duration(in.TimeoutSeconds) * time.Second
			if timeout <= 0 {
				timeout = defaultWait
			}
			tasks, err := m.Wait(ctx, in.TaskIDs, min(timeout, maxWait))
			if err != nil {
				return waitOutput{}, err
			}
			out := waitOutput{Tasks: make([]Status, 0, len(tasks))}
			for _, t := range tasks {
				out.Tasks = append(out.Tasks, statusOf(t))
			}
			return out, nil
		}, argon.ReadOnly(), argon.ToolTimeout(waitToolTimeout))

	argon.Tool(s, "list_tasks",
		"List tasks, one line each. This instance's tasks only: other Claude Code windows run their own Talos.",
		func(_ context.Context, in listInput) (listOutput, error) {
			out := listOutput{Tasks: []string{}}
			for _, t := range m.List(task.State(in.State)) {
				out.Tasks = append(out.Tasks, summaryLine(t))
			}
			return out, nil
		}, argon.ReadOnly())
}

func summaryLine(t *task.Task) string {
	parts := []string{t.ID, string(t.State), t.Branch, fmt.Sprintf("steps=%d", t.Steps)}
	if t.Error != "" {
		parts = append(parts, "error="+t.Error)
	}
	return strings.Join(parts, " ")
}
