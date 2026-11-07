// Command talos is a stdio MCP server that runs OpenCode sessions as sub-agents
// of an orchestrator such as Claude Code.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"go.xchunk.org/argon"
	"go.xchunk.org/talos/internal/config"
	"go.xchunk.org/talos/internal/opencode"
	"go.xchunk.org/talos/internal/task"
	"go.xchunk.org/talos/internal/tools"
)

const instructions = `Talos runs OpenCode sub-agents, each on its own branch and git worktree.
Delegate with start_task, block on wait_task, then read get_result and, when reviewing, get_diff.
Answer pending permissions with respond_permission and sub-agent questions with send_message.
Finish with cleanup_task: merge=true brings the work into the source checkout. Talos never merges on its own.`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "talos:", err)
		os.Exit(1)
	}
}

func run() error {
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := config.Load(os.Getenv, workDir)
	if err != nil {
		return err
	}
	store, err := task.NewFileStore(filepath.Join(cfg.StateDir, "tasks"))
	if err != nil {
		return err
	}
	// Stdout carries the MCP protocol, so logs must stay on stderr.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	conn := opencode.NewConnection(opencode.Locator{
		URL:      cfg.OpenCodeURL,
		Password: cfg.OpenCodePassword,
	}, cfg.Autostart)
	manager := task.NewManager(task.Options{
		Backend:           conn,
		Store:             store,
		Roots:             cfg.AllowedRoots,
		WorktreeDir:       filepath.Join(cfg.StateDir, "worktrees"),
		MaxParallel:       cfg.MaxParallel,
		DefaultModel:      cfg.DefaultModel,
		Timeout:           cfg.TaskTimeout,
		MaxSteps:          cfg.MaxSteps,
		PermissionTimeout: cfg.PermissionTimeout,
		ResultLimit:       cfg.ResultLimit,
		ShellDeny:         cfg.ShellDeny,
		ShellAsk:          cfg.ShellAsk,
		Logger:            logger,
	})
	manager.Adopt()

	server := argon.New("talos", argon.Instructions(instructions), argon.Logger(logger))
	tools.Register(server, manager)
	tools.RegisterServer(server, conn)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	engineCtx, stopEngine := context.WithCancel(ctx)
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		manager.Run(engineCtx)
	}()

	serveErr := server.RunStdio(ctx)
	// Stopping leaves sessions running on the service. The next instance adopts
	// them, which is what lets tasks survive an orchestrator restart.
	stopEngine()
	<-engineDone
	if ctx.Err() != nil {
		return nil
	}
	return serveErr
}
