# Talos: agent instructions

Talos is a stdio MCP server, written in Go, that lets an orchestrator such as
Claude Code run OpenCode sessions as its sub-agents and manage them.

## Commits

- Make atomic commits: one logical change per commit. Do not mix refactoring,
  features and formatting.
- Every commit must build and pass checks on its own.
- Use Conventional Commits with an optional package scope, for example
  `feat(task): add the scheduler` or `docs: describe the event stream`.

## Language

Everything in the repository is in English: code, comments, documentation,
commit messages and tool descriptions.

## Comments and documentation

- Write comments the way a careful engineer would for a colleague. Explain why
  the code does something, name the constraint or the surprise behind it, and
  skip anything the code already says.
- Avoid boilerplate and filler. A comment that only restates the next line
  should not exist.
- Document the code well: every package has a package comment, and every
  exported identifier has a doc comment that states its contract, including
  concurrency and error behavior where they matter.
- When behavior depends on something OpenCode does, say so in the comment and
  keep the details in `docs/opencode-api.md`.

## Project documents

- `docs/opencode-api.md`: the verified OpenCode API contract. Update it when
  OpenCode behaves differently from what it says.

## Working with OpenCode

Ordinary `opencode` CLI commands start the user's background service as a side
effect, and live sessions write to the user's database. Run experiments and
integration tests against a foreground `opencode serve` with isolated `XDG_*`
directories.

## Keeping this file current

Update `AGENTS.md` whenever development changes what it says or adds something
a future contributor needs to know, such as new commands, layout, conventions
or decisions. Make the update in the same commit as the change that caused it.

## Checks

Every commit must pass:

```sh
gofmt -l . && go build ./... && go vet ./... && go test -race ./...
```

`gofmt -l` must print nothing.

## Layout

Dependencies point one way: `cmd/talos → internal/tools → internal/task →
{internal/opencode, internal/gitwt}`, with `internal/config` read by `cmd/talos`.
Only `cmd/talos` and `internal/tools` import Argon, so task logic is testable
without MCP. Task tests use a fake `task.Backend` and real temporary git
repositories.
