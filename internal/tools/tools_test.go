package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.xchunk.org/argon"
	"go.xchunk.org/argon/argontest"
	"go.xchunk.org/talos/internal/opencode"
	"go.xchunk.org/talos/internal/task"
)

// backend fakes OpenCode. Embedding the interface means a test that reaches an
// unexpected call fails loudly with a nil dereference.
type backend struct {
	task.Backend
	mu       sync.Mutex
	handle   func(opencode.Event)
	messages []opencode.Message
	replies  []string
	prompts  []string
	active   map[string]bool
}

func (b *backend) Ensure(context.Context) (opencode.Info, error) {
	return opencode.Info{Version: "2.0.32"}, nil
}

func (b *backend) CreateSession(context.Context, opencode.CreateSessionRequest) (opencode.Session, error) {
	return opencode.Session{ID: "ses_1"}, nil
}

func (b *backend) Prompt(_ context.Context, _, text, delivery string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prompts = append(b.prompts, delivery+":"+text)
	return "msg", nil
}

func (b *backend) Interrupt(context.Context, string) (bool, error) { return true, nil }

func (b *backend) ActiveSessions(context.Context) (map[string]bool, error) {
	return map[string]bool{"ses_1": true}, nil
}

func (b *backend) Permissions(context.Context, string) ([]opencode.PermissionRequest, error) {
	return nil, nil
}

func (b *backend) ChildSessions(context.Context, string) ([]opencode.Session, error) {
	return nil, nil
}

func (b *backend) RecentMessages(context.Context, string, int) ([]opencode.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.messages, nil
}

func (b *backend) ReplyPermission(_ context.Context, session, request, decision, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.replies = append(b.replies, session+"/"+request+"/"+decision)
	return nil
}

func (b *backend) Stream(ctx context.Context, _ opencode.StreamOptions, handle func(opencode.Event)) error {
	b.mu.Lock()
	b.handle = handle
	b.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

// emit delivers an event once the manager's stream is connected.
func (b *backend) emit(t *testing.T, typ string, data map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(data)
	eventually(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.handle != nil
	})
	b.mu.Lock()
	handle := b.handle
	b.mu.Unlock()
	handle(opencode.Event{Type: typ, Data: raw})
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type fixture struct {
	t       *testing.T
	s       *argon.Server
	m       *task.Manager
	backend *backend
	repo    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	state := t.TempDir()
	store, err := task.NewFileStore(filepath.Join(state, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	b := &backend{}
	m := task.NewManager(task.Options{
		Backend:           b,
		Store:             store,
		Roots:             []string{repo},
		WorktreeDir:       filepath.Join(state, "worktrees"),
		MaxParallel:       2,
		Timeout:           time.Hour,
		MaxSteps:          100,
		PermissionTimeout: time.Hour,
		ResultLimit:       50,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	s := argon.New("test")
	Register(s, m)
	return &fixture{t: t, s: s, m: m, backend: b, repo: repo}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// start runs start_task and waits for the prompt to be delivered.
func (f *fixture) start() startOutput {
	f.t.Helper()
	out, err := argontest.Call[startOutput](f.t, f.s, "start_task", map[string]any{"directory": f.repo, "prompt": "go"})
	if err != nil {
		f.t.Fatal(err)
	}
	eventually(f.t, func() bool {
		tk, err := f.m.Get(out.TaskID)
		return err == nil && tk.Prompt == ""
	})
	return out
}

func TestStartAndGet(t *testing.T) {
	f := newFixture(t)
	started := f.start()
	if started.Branch != "talos/"+started.TaskID || started.Worktree == "" {
		t.Errorf("started = %+v", started)
	}
	status, err := argontest.Call[Status](t, f.s, "get_task", map[string]any{"task_id": started.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "running" {
		t.Errorf("status = %+v", status)
	}
}

func TestSchemaValidation(t *testing.T) {
	f := newFixture(t)
	if _, err := argontest.Call[startOutput](t, f.s, "start_task", map[string]any{"directory": f.repo}); err == nil {
		t.Error("a missing prompt must be rejected")
	}
	if _, err := argontest.Call[Status](t, f.s, "get_task", map[string]any{"task_id": "nope"}); err == nil {
		t.Error("an unknown task must be an error")
	}
}

func TestWaitSurfacesPermissionsInline(t *testing.T) {
	f := newFixture(t)
	started := f.start()
	f.backend.emit(t, opencode.EventPermissionAsked, map[string]any{
		"id": "per_1", "sessionID": "ses_1", "action": "shell", "resources": []string{"ls"},
	})
	out, err := argontest.Call[waitOutput](t, f.s, "wait_task", map[string]any{"task_ids": []string{started.TaskID}, "timeout_seconds": 5})
	if err != nil {
		t.Fatal(err)
	}
	got := out.Tasks[0]
	if got.State != "waiting_permission" || got.Pending[0].RequestID != "per_1" || got.Pending[0].Resources[0] != "ls" {
		t.Errorf("status = %+v", got)
	}
}

func TestWaitTimesOutWithCurrentState(t *testing.T) {
	f := newFixture(t)
	started := f.start()
	out, err := argontest.Call[waitOutput](t, f.s, "wait_task", map[string]any{"task_ids": []string{started.TaskID}, "timeout_seconds": 1})
	if err != nil || out.Tasks[0].State != "running" {
		t.Errorf("out = %+v, err = %v", out, err)
	}
}

func TestListTasks(t *testing.T) {
	f := newFixture(t)
	started := f.start()
	out, err := argontest.Call[listOutput](t, f.s, "list_tasks", map[string]any{})
	if err != nil || len(out.Tasks) != 1 || !strings.HasPrefix(out.Tasks[0], started.TaskID+" running") {
		t.Errorf("out = %+v, err = %v", out, err)
	}
	out, err = argontest.Call[listOutput](t, f.s, "list_tasks", map[string]any{"state": "failed"})
	if err != nil || len(out.Tasks) != 0 {
		t.Errorf("out = %+v, err = %v", out, err)
	}
}
