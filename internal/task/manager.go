package task

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.xchunk.org/talos/internal/gitwt"
	"go.xchunk.org/talos/internal/opencode"
)

// ErrNotFound is returned for an unknown task ID.
var ErrNotFound = errors.New("task not found")

// turnSetupTimeout bounds creating a session and delivering a prompt. Neither
// waits for the model, so a call this slow means the server is in trouble.
const turnSetupTimeout = 60 * time.Second

// Backend is the part of OpenCode the engine uses. *opencode.Connection
// implements it.
type Backend interface {
	Ensure(ctx context.Context) (opencode.Info, error)
	CreateSession(ctx context.Context, req opencode.CreateSessionRequest) (opencode.Session, error)
	Prompt(ctx context.Context, sessionID, text, delivery string) (string, error)
	Interrupt(ctx context.Context, sessionID string) (bool, error)
	ReplyPermission(ctx context.Context, sessionID, requestID, decision, message string) error
	ActiveSessions(ctx context.Context) (map[string]bool, error)
	RecentMessages(ctx context.Context, sessionID string, limit int) ([]opencode.Message, error)
	Permissions(ctx context.Context, sessionID string) ([]opencode.PermissionRequest, error)
	ChildSessions(ctx context.Context, parentID string) ([]opencode.Session, error)
	CancelForm(ctx context.Context, sessionID, formID string) error
	Stream(ctx context.Context, opts opencode.StreamOptions, handle func(opencode.Event)) error
}

// Options configures a Manager.
type Options struct {
	Backend Backend
	Store   Store
	// Roots are the symlink-resolved directories tasks may run in. OpenCode
	// accepts any directory, so this check is the only one there is.
	Roots []string
	// WorktreeDir holds one worktree per task.
	WorktreeDir string

	MaxParallel       int
	DefaultModel      string
	Timeout           time.Duration
	MaxSteps          int
	PermissionTimeout time.Duration
	ResultLimit       int
	ShellDeny         []string

	// PID identifies this instance as task owner. Default os.Getpid().
	PID int
	// Now is the clock. Default time.Now.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Manager runs tasks. All methods are safe for concurrent use.
//
// Lock discipline: mu is held only for in-memory work, never across network or
// disk I/O. The event reader takes it for every event, and a slow reader makes
// OpenCode fail the stream.
type Manager struct {
	opts   Options
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	tasks   map[string]*Task
	changed chan struct{} // closed and replaced on every change
	dirty   map[string]struct{}

	persist chan struct{}

	streamOnce  sync.Once
	versionOnce sync.Once

	// interruptSent records when a watchdog's interrupt went out, per task. It is
	// not persisted: after a restart the deadline simply starts over.
	interruptSent map[string]time.Time

	// reconcileNow asks reconcileLoop for a pass without blocking the caller.
	reconcileNow chan struct{}

	// cleaning marks tasks in the middle of Cleanup, which must not be restarted.
	cleaning map[string]bool
}

// NewManager returns a manager. Call Adopt and then Run to start it.
func NewManager(opts Options) *Manager {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.PID == 0 {
		opts.PID = os.Getpid()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		opts:    opts,
		ctx:     ctx,
		cancel:  cancel,
		tasks:   make(map[string]*Task),
		changed: make(chan struct{}),
		dirty:   make(map[string]struct{}),
		persist: make(chan struct{}, 1),

		interruptSent: make(map[string]time.Time),
		reconcileNow:  make(chan struct{}, 1),
		cleaning:      make(map[string]bool),
	}
}

// Run does the background work until ctx is done, then waits for in-flight work
// and writes out pending state. Sessions keep running on the server.
func (m *Manager) Run(ctx context.Context) {
	stop := context.AfterFunc(ctx, m.cancel)
	defer stop()

	m.dispatch()
	m.requestReconcile()

	m.wg.Go(m.writeLoop)
	m.wg.Go(m.watchdogLoop)
	m.wg.Go(m.reconcileLoop)
	<-m.ctx.Done()
	m.wg.Wait()
	m.flush()
}

// StartRequest describes a new task. Zero optional fields select the defaults.
type StartRequest struct {
	Directory string
	Prompt    string
	Model     string
	Agent     string
	BaseRef   string
	Timeout   time.Duration
	MaxSteps  int
}

// Start validates the request and creates the worktree synchronously, so errors
// reach the caller right away, and returns the task as queued. The session is
// created later, when a slot is free.
func (m *Manager) Start(ctx context.Context, req StartRequest) (*Task, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("prompt is empty")
	}
	model := req.Model
	if model == "" {
		model = m.opts.DefaultModel
	}
	if _, err := parseModel(model); err != nil {
		return nil, err
	}
	if req.Timeout < 0 || req.MaxSteps < 0 {
		return nil, errors.New("timeout and max_steps must not be negative")
	}
	source, err := m.resolveSource(ctx, req.Directory)
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.opts.WorktreeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create worktree directory: %w", err)
	}
	worktree := filepath.Join(m.opts.WorktreeDir, id)
	branch := "talos/" + id
	base, err := gitwt.Create(ctx, source, worktree, branch, req.BaseRef)
	if err != nil {
		return nil, err
	}

	t := &Task{
		ID:         id,
		State:      StateQueued,
		Directory:  source,
		Worktree:   worktree,
		Branch:     branch,
		BaseCommit: base,
		Prompt:     req.Prompt,
		Model:      model,
		Agent:      req.Agent,
		Timeout:    cmpOr(req.Timeout, m.opts.Timeout),
		MaxSteps:   cmpOr(req.MaxSteps, m.opts.MaxSteps),
		OwnerPID:   m.opts.PID,
		CreatedAt:  m.opts.Now(),
		Version:    1,
	}
	// Saved before anyone learns the ID, so a crash cannot orphan the worktree.
	if err := m.opts.Store.Save(t); err != nil {
		removeErr := gitwt.Remove(ctx, source, worktree, branch)
		return nil, errors.Join(err, removeErr)
	}
	m.mu.Lock()
	m.tasks[id] = t
	snapshot := t.Clone()
	m.notifyLocked()
	m.mu.Unlock()
	m.dispatch()
	return snapshot, nil
}

func cmpOr[T comparable](value, fallback T) T {
	var zero T
	if value == zero {
		return fallback
	}
	return value
}

// resolveSource returns the top level of the repository containing dir, after
// checking that it lies inside an allowed root. Both the directory and the
// repository are checked: an allowed subdirectory of a larger repository would
// otherwise expose, and later merge into, everything above it.
func (m *Manager) resolveSource(ctx context.Context, dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("directory %q is not absolute", dir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolve directory: %w", err)
	}
	root, err := gitwt.Root(ctx, resolved)
	if err != nil {
		return "", fmt.Errorf("directory %q is not in a git repository: %w", dir, err)
	}
	if !m.allowed(resolved) || !m.allowed(root) {
		return "", fmt.Errorf("repository %q is outside the allowed roots (TALOS_ALLOWED_ROOTS)", root)
	}
	return root, nil
}

func (m *Manager) allowed(path string) bool {
	for _, root := range m.opts.Roots {
		if path == root || strings.HasPrefix(path, strings.TrimRight(root, string(filepath.Separator))+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// parseModel splits "provider/model". An empty string yields nil, leaving the
// choice to OpenCode's own default. Model IDs may contain further slashes.
func parseModel(model string) (*opencode.ModelRef, error) {
	if model == "" {
		return nil, nil
	}
	provider, id, ok := strings.Cut(model, "/")
	if !ok || provider == "" || id == "" {
		return nil, fmt.Errorf("model %q is not provider/model", model)
	}
	return &opencode.ModelRef{ID: id, ProviderID: provider}, nil
}

// sessionRules builds a session's ruleset. Order matters, because a later rule
// overrides an earlier one. Denying external_directory stops file tools at the
// worktree boundary, and denying question keeps a sub-agent from blocking on a
// form nobody would see; it ends its turn with the question instead.
func (m *Manager) sessionRules() []opencode.PermissionRule {
	rules := []opencode.PermissionRule{
		{Action: "*", Resource: "*", Effect: opencode.EffectAllow},
		{Action: "external_directory", Resource: "*", Effect: opencode.EffectDeny},
		{Action: "question", Resource: "*", Effect: opencode.EffectDeny},
	}
	for _, pattern := range m.opts.ShellDeny {
		rules = append(rules, opencode.PermissionRule{Action: "shell", Resource: pattern, Effect: opencode.EffectDeny})
	}
	return rules
}

// dispatch promotes the oldest queued tasks while slots are free. A promoted task
// counts against the limit from this moment, before its session exists.
func (m *Manager) dispatch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return
	}
	busy := 0
	var queued []*Task
	for _, t := range m.tasks {
		switch {
		case t.State.holdsSlot():
			busy++
		case t.State == StateQueued:
			queued = append(queued, t)
		}
	}
	slices.SortFunc(queued, func(a, b *Task) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for _, t := range queued {
		if busy >= m.opts.MaxParallel {
			return
		}
		if err := t.transition(StateRunning, m.opts.Now()); err != nil {
			m.opts.Logger.Error("dispatch", "error", err)
			continue
		}
		busy++
		t.Turn++
		t.Steps = 0
		t.Error = ""
		t.TurnStartedAt = m.opts.Now()
		t.PromptSentAt = time.Time{}
		m.touchLocked(t)
		snapshot := t.Clone()
		m.wg.Go(func() { m.startTurn(snapshot) })
	}
}

// startTurn creates the session if needed and sends the prompt. The prompt stays
// on the task until it is delivered, so a crash before that can start over.
func (m *Manager) startTurn(snapshot *Task) {
	ctx, cancel := context.WithTimeout(m.ctx, turnSetupTimeout)
	defer cancel()
	id, turn := snapshot.ID, snapshot.Turn

	if err := m.connect(ctx); err != nil {
		m.failTurn(id, turn, fmt.Errorf("OpenCode is not available: %w", err))
		return
	}
	sessionID := snapshot.SessionID
	if sessionID == "" {
		model, err := parseModel(snapshot.Model)
		if err != nil {
			m.failTurn(id, turn, err)
			return
		}
		session, err := m.opts.Backend.CreateSession(ctx, opencode.CreateSessionRequest{
			Title:       "talos " + id,
			Agent:       snapshot.Agent,
			Model:       model,
			Location:    &opencode.Location{Directory: snapshot.Worktree},
			Permissions: m.sessionRules(),
		})
		if err != nil {
			m.failTurn(id, turn, fmt.Errorf("create session: %w", err))
			return
		}
		sessionID = session.ID
	}
	// Recorded before the prompt is sent, because the first events can arrive
	// before the call returns.
	proceed := m.update(id, func(t *Task) bool {
		if t.Turn != turn || t.State.Finished() {
			return false
		}
		t.SessionID = sessionID
		t.PromptSentAt = m.opts.Now()
		return true
	})
	if !proceed {
		return
	}
	if _, err := m.opts.Backend.Prompt(ctx, sessionID, snapshot.Prompt, ""); err != nil {
		m.failTurn(id, turn, fmt.Errorf("send prompt: %w", err))
		return
	}
	m.update(id, func(t *Task) bool {
		if t.Turn == turn {
			t.Prompt = ""
		}
		return true
	})
}

func (m *Manager) failTurn(id string, turn int, cause error) {
	// Shutting down cancels setup calls. That says nothing about the task, and
	// adoption picks it up on the next start.
	if m.ctx.Err() != nil {
		return
	}
	m.update(id, func(t *Task) bool {
		if t.Turn != turn || t.State.Finished() {
			return false
		}
		t.Error = cause.Error()
		m.finishLocked(t, StateFailed)
		return true
	})
}

// finishLocked moves t to a terminal state. It logs instead of failing because an
// illegal edge here means a late event for a task that already moved on.
func (m *Manager) finishLocked(t *Task, state State) {
	if err := t.transition(state, m.opts.Now()); err != nil {
		m.opts.Logger.Debug("ignored transition", "error", err)
	}
}

// update applies fn to a task and returns fn's result. Returning false means fn
// changed nothing. fn runs under the lock and must not do I/O. Any change can
// free a slot, so it is followed by a dispatch.
func (m *Manager) update(id string, fn func(*Task) bool) bool {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return false
	}
	changed := fn(t)
	if changed {
		m.touchLocked(t)
	}
	m.mu.Unlock()
	if changed {
		m.dispatch()
	}
	return changed
}

func (m *Manager) touchLocked(t *Task) {
	t.Version++
	m.dirty[t.ID] = struct{}{}
	m.notifyLocked()
	select {
	case m.persist <- struct{}{}:
	default:
	}
}

func (m *Manager) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// writeLoop saves changed tasks away from the event path. Repeated changes to a
// task coalesce into one write, so a slow disk costs freshness, not the stream.
func (m *Manager) writeLoop() {
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.persist:
			m.flush()
		}
	}
}

func (m *Manager) flush() {
	m.mu.Lock()
	var pending []*Task
	var deleted []string
	for id := range m.dirty {
		if t, ok := m.tasks[id]; ok {
			pending = append(pending, t.Clone())
		} else {
			deleted = append(deleted, id)
		}
	}
	clear(m.dirty)
	m.mu.Unlock()

	for _, t := range pending {
		if err := m.opts.Store.Save(t); err != nil {
			m.opts.Logger.Error("persist task", "task", t.ID, "error", err)
		}
	}
	for _, id := range deleted {
		if err := m.opts.Store.Delete(id); err != nil {
			m.opts.Logger.Error("delete task", "task", id, "error", err)
		}
	}
}

// Get returns a copy of a task, or ErrNotFound.
func (m *Manager) Get(id string) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return t.Clone(), nil
}

// List returns copies of all tasks, oldest first, limited to state if non-empty.
func (m *Manager) List(state State) []*Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		if state == "" || t.State == state {
			out = append(out, t.Clone())
		}
	}
	slices.SortFunc(out, func(a, b *Task) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// Wait blocks until any listed task is finished or waiting on a permission, or
// until timeout or ctx ends it, and returns the listed tasks in request order. It
// keeps no per-caller state: it answers from current state, and callers drop
// finished IDs from their next call, so a change between two calls cannot be
// missed. A timeout is not an error.
func (m *Manager) Wait(ctx context.Context, ids []string, timeout time.Duration) ([]*Task, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		m.mu.Lock()
		out := make([]*Task, 0, len(ids))
		ready := false
		for _, id := range ids {
			t, ok := m.tasks[id]
			if !ok {
				m.mu.Unlock()
				return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
			}
			out = append(out, t.Clone())
			ready = ready || t.State.Finished() || t.State == StateWaitingPermission
		}
		changed := m.changed
		m.mu.Unlock()
		if ready {
			return out, nil
		}
		select {
		case <-changed:
		case <-timer.C:
			return out, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.ctx.Done():
			return nil, m.ctx.Err()
		}
	}
}
