# Talos

Talos is a stdio MCP server that lets an orchestrator such as Claude Code run
[OpenCode](https://opencode.ai) sessions as sub-agents. Each task gets its own git
worktree and branch. The orchestrator follows tasks without flooding its context,
answers their permission requests, reviews the result and decides whether to merge.

Status: prototype, tested against OpenCode 2.0.32.

## Security

**Sub-agents run shell commands with your privileges. Talos does not sandbox them.**

- File tools are confined to the worktree, using `deny external_directory`.
- The shell is not confined. A command can read and write anything you can.
- The shell deny list guards against accidents, not against a determined or
  compromised model. It matches the command line, so `bash -c 'git push'` and
  `rm -r -f` get past it.
- Real isolation needs an OS-level sandbox around the OpenCode server.
- Free `opencode/*` models may keep prompts and code. Choose a model deliberately
  for private work.

## Setup

Requires Go 1.26, git and OpenCode 2.x.

```sh
go install go.xchunk.org/talos/cmd/talos@latest
claude mcp add talos -- talos
```

Tasks may only run in repositories under `TALOS_ALLOWED_ROOTS`, which defaults to
the directory Talos was launched from.

## How a task runs

1. `start_task` creates the worktree and returns a task ID at once. The worktree
   starts from a commit, so uncommitted changes in your checkout are not visible.
2. `wait_task` blocks until a task finishes or needs a permission answer.
3. `get_result` returns the final message and diff stat, and `get_diff` the patch.
4. `send_message` continues the session, and `respond_permission` answers a request.
5. `cleanup_task` removes the worktree and branch. With `merge=true` it first merges
   into the source checkout's current branch. Talos never merges on its own.

`succeeded` only means the turn ended cleanly. A model can carry on after a denied
action, so check `denied_actions` and the diff.

## Tools

| Tool | Purpose |
|------|---------|
| `start_task` | Start a sub-agent in a new worktree |
| `get_task` | One task's state, steps, cost and progress line |
| `wait_task` | Block until a task finishes or waits on a permission |
| `list_tasks` | One line per task |
| `get_result` | Final message, diff stat, usage, `denied_actions` |
| `get_diff` | Patch against the base commit, paged |
| `get_transcript` | Compact transcript, paged |
| `send_message` | Follow up, or steer a running turn |
| `respond_permission` | Answer a pending request |
| `abort_task` | Stop a task and keep its worktree |
| `cleanup_task` | Merge if asked, then remove the worktree and branch |
| `server_status` | Check OpenCode without starting it |
| `start_server` | Start the managed OpenCode service |

## Configuration

| Variable | Default | Purpose |
|----------|---------|---------|
| `TALOS_OPENCODE_URL`, `TALOS_OPENCODE_PASSWORD` | From the managed service | Attach to a specific server |
| `TALOS_AUTOSTART` | `true` | Start the service when needed |
| `TALOS_ALLOWED_ROOTS` | Launch directory | Colon-separated project roots |
| `TALOS_MAX_PARALLEL` | `3` | Concurrent tasks, per Talos instance |
| `TALOS_DEFAULT_MODEL` | OpenCode's default | `provider/model` |
| `TALOS_TASK_TIMEOUT` | `30m` | Wall-clock limit per turn |
| `TALOS_MAX_STEPS` | `200` | Step limit per turn |
| `TALOS_PERMISSION_TIMEOUT` | `10m` | Limit on waiting for an answer |
| `TALOS_RESULT_LIMIT` | `8000` | Bytes of final message returned |
| `TALOS_SHELL_DENY` | See `internal/config` | Comma-separated shell patterns to block |
| `TALOS_SHELL_ASK` | Empty | Comma-separated shell patterns that need approval |
| `TALOS_STATE_DIR` | `$XDG_STATE_HOME/talos` | Task files and worktrees |

Everything not matched by these patterns is allowed without asking, so a task only
reaches `waiting_permission` if `TALOS_SHELL_ASK` is set.

## Known limits

- Timeouts are enforced only while Talos runs. Sessions keep working on the service
  between an orchestrator exit and the next start, which adopts them.
- `TALOS_MAX_PARALLEL` is per instance, not global across Claude Code windows.
- A task is adopted only at startup, and only if its previous owner has exited.
- OpenCode's event stream is undocumented and can change between releases. Talos
  ignores unknown events and treats REST as the source of truth. See
  [docs/opencode-api.md](docs/opencode-api.md).
