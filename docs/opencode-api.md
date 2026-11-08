# OpenCode v2 API notes

This is the contract Talos relies on. It was checked against OpenCode 2.0.32 on
2026-11-07, using the server's own OpenAPI document (`GET /openapi.json`) and
live sessions against a free model.

Most guides online describe the 1.x API (`/event`, `/session/:id/prompt_async`).
Those routes do not exist in 2.x. The OpenAPI document does not type individual
events either, so everything under "Event stream" comes from captured traffic
and may change without notice between releases.

## Running the server

| Mode | Command | Notes |
|------|---------|-------|
| Managed service | `opencode service start` | Idempotent, takes about 0.3s when already running, prints the URL. Outlives the caller. |
| Status | `opencode service status` | Prints the URL, or `stopped`. |
| Password | `opencode service get password` | Prints the raw password. |
| Foreground | `opencode serve --hostname H --port P` | Takes `OPENCODE_SERVER_PASSWORD`, or generates and prints one. |
| Registration file | `$XDG_STATE_HOME/opencode/service.json` | Mode 0600. Holds `id`, `version`, `url`, `pid`, `password`. Removed by `service stop`. Reading it has no side effects, unlike the CLI. |

**Ordinary CLI commands start the service as a side effect.** `opencode models`
left a `serve --service` process running after `service status` had reported
`stopped`. Tests must set their own `XDG_*` directories and use a foreground
server so they never touch the user's service.

**The managed service listens on a fixed port, 49374 by default, whatever the
`XDG_*` directories say.** A service started with isolated directories fails
with "Managed service port 49374 on 127.0.0.1 is already in use" while another
service holds the port. An isolated service needs its own port, set with
`opencode service set port <port>` under the same `XDG_*` directories. Verified
with OpenCode 2.0.32.

`opencode serve --stdio` looks attractive because it stops when stdin closes,
but according to its bundled source it discards any configured password and
generates a random one it does not print, so a client cannot authenticate.

## Authentication

HTTP Basic with the username `opencode`. An unauthenticated `/api/*` request gets
401. Paths outside `/api/` and `/auth/` fall through to the web UI and answer 200
with HTML, so `/health` and `/doc` say nothing about server health. Use
`GET /api/info`, which returns `version`, `pid`, `urls` and `paths`.

## Operations Talos uses

| Purpose | Operation | Notes |
|---------|-----------|-------|
| Health and version | `GET /api/info` | |
| Create a session | `POST /api/session` | Body: `title`, `agent`, `model{id,providerID,variant?}`, `location{directory}`, `metadata`, `permissions`. |
| Send a prompt | `POST /api/session/{id}/prompt` | Body: `text`, `delivery` (`steer` or `queue`). Returns at once with the message ID. Asynchronous by design. |
| Interrupt | `POST /api/session/{id}/interrupt` | Returns `{interrupted: bool}`. Idle is a harmless `false`. |
| Session state | `GET /api/session/{id}` | `outcome` and `time.idle` are cleared while a turn runs. |
| Running sessions | `GET /api/session/active` | A map from session ID to `{type: "running"}`. A session stays listed while waiting on a permission. |
| Child sessions | `GET /api/session?parentID={id}` | Direct children only. Recovers children whose `session.created` was lost. |
| Messages | `GET /api/session/{id}/message?order=desc&limit=N` | Cursor paging. `limit` above 200 answers 400. |
| Pending permissions | `GET /api/session/{id}/permission` | Use the per-session route. The global `/api/permission/request` returned an empty list while a request was pending. |
| Answer a permission | `POST /api/session/{id}/permission/{requestID}/reply` | Body: `decision` (`once`, `always`, `reject`), `message?`. The session in the path must own the request, which may be a child session. |
| Pending forms | `GET /api/session/{id}/form` | Created by the `question` tool. |
| Cancel a form | `DELETE /api/session/{id}/form/{formID}` | Interrupts the turn with reason `shutdown`. |

Successful responses usually wrap the payload in `{data: ...}`, but `/api/info`
and `interrupt` answer bare. Errors are JSON
with a `_tag`, for example `{"_tag":"UnauthorizedError","message":"..."}`.

Nothing validates a session up front. `POST /api/session` accepts a directory
that does not exist and a model that does not exist. A bad model only surfaces
later as `session.execution.failed` with type `provider.no-route`. Talos must
validate directories itself.

`POST /api/worktree` needs a branch that already exists and runs project setup
scripts. Talos runs `git worktree` directly instead. A session located in a
linked worktree resolves to the same `projectID` as the main checkout and works
normally.

`GET /api/session/{id}/diff` covers a single turn and compares snapshots, so it
is not the task's total change. Talos computes diffs with git against the
recorded base commit.

## Event stream

`GET /api/event` is Server-Sent Events across all sessions. Each payload is a
`data:` line holding one JSON object:

```json
{"id":"evt_...","created":1794025366416,"type":"session.execution.started",
 "location":{"directory":"/path"},"data":{"sessionID":"ses_..."},
 "durable":{"aggregateID":"ses_...","seq":2,"version":1}}
```

`location` and `durable` are optional. Heartbeats arrive as `: heartbeat`
comments. The first event is `server.connected`.

The stream is volatile by contract: a slow consumer overflows and fails it, and
events sent during a disconnect are lost. The reader must never block on
downstream work, and clients must reconcile through REST after a reconnect.

| Event | Fields in `data` | Meaning for Talos |
|-------|------------------|-------------------|
| `session.created` | `sessionID`, `parentID?` | With `parentID`, a child session from the `task` tool. Map it to the parent's task. |
| `session.execution.started` | `sessionID` | A turn began. |
| `session.execution.succeeded` | `sessionID` | Turn ended. Also fires for child sessions, so only the root ends a task. |
| `session.execution.failed` | `sessionID`, `error{type,message}` | Turn failed. |
| `session.execution.interrupted` | `sessionID`, `reason` | Reasons seen: `user`, `shutdown`. |
| `session.step.started` | `sessionID`, `assistantMessageID`, `agent`, `model` | One model step. Counted for `max_steps`. |
| `session.step.ended` | `finish`, `cost`, `tokens`, `snapshot` | |
| `session.step.failed` | `error{type,message}` | |
| `session.tool.input.started` | `id`, `name` | The tool name appears here, for the progress line. |
| `session.tool.called` | `id`, `input` | |
| `session.tool.success` | `id`, `content` | |
| `session.tool.failed` | `id`, `error{type,message}` | `permission.rejected` means a rule or reply denied the call. |
| `session.usage.updated` | `cost`, `tokens` | Cumulative for that session, so a task's total is a sum over its sessions' latest values. |
| `session.text.ended` | `text` | Assistant text block finished. |
| `permission.asked` | `id`, `sessionID`, `action`, `resources`, `save` | Blocks the turn until answered. |
| `permission.replied` | `sessionID`, `requestID`, `reply` | |
| `form.created` | `form{id,sessionID,title,fields}` | A question for the user. Blocks the turn. |
| `form.cancelled` | `id`, `sessionID` | |

Talos ignores everything else, including `session.text.delta` and
`session.reasoning.delta`, which are the high-volume events.

## Messages

`GET .../message` returns a timeline of typed items. Those Talos reads:

- `user`: `text`, `time.created`.
- `assistant`: `content[]` of `text`, `reasoning` and `tool` blocks, plus
  `finish`, `cost`, `tokens`. The final answer is the last assistant message with
  a `text` block; reasoning must be excluded.
- `idle`: `outcome` (`succeeded`, `failed`, `interrupted`), `error?`,
  `time.created`. It closes a turn, and is how reconciliation learns the result
  of a turn whose events were missed.

## Permissions

`permissions` on session creation is a list of
`{action, resource, effect}` rules, with effect `allow`, `deny` or `ask`. **A later
rule overrides an earlier one**, so general rules go first. Child sessions inherit
the rules.

| Action | Resource | Observed |
|--------|----------|----------|
| `edit` | Path relative to the workspace, or absolute outside it | Covers the write tool. |
| `read` | Path | |
| `shell` | The whole command line | Asking offers `save` patterns such as `echo *`. |
| `external_directory` | `<dir>/*` | Checked by file tools when a path leaves the session's location. |
| `question` | `*` | Deny it and the tool is not offered to the model. |
| `task` | Sub-agent | Not exercised on its own. |

What the tests established:

- With no rules, or with only `allow *`, the write tool **wrote outside the
  session directory without asking**.
- `deny external_directory *` stops file tools at the workspace boundary while
  writes inside keep working.
- It does **not** affect `shell`. `echo x > /elsewhere/file` still ran, directly
  and through `bash -c`.
- A rule on `edit` with an absolute path pattern does not match paths inside the
  workspace, because those resources are relative. Do not scope `edit` that way.

Shell deny patterns match the full command line, including when the target is
part of a compound command:

| Command, under `deny git push*` and `deny rm -rf *` | Result |
|------------------------------------------------------|--------|
| `git push origin x` | Blocked |
| `rm -rf file` | Blocked |
| `true && git push origin x` | Blocked |
| `true; rm -rf file` | Blocked |
| `bash -c 'git push origin x'` | **Ran** |
| `sh -c 'rm -rf file'` | **Ran** |
| `rm -r -f file` | **Ran** |

A deny list therefore prevents accidents but does not contain a determined or
compromised model. OpenCode has no sandbox of its own, and real isolation would
need an OS-level one around the server.

A session that ends with a rejected permission still reports `outcome:
succeeded`, because the model carried on and finished its turn. Success means
only that the turn ended cleanly, not that the work got done.
