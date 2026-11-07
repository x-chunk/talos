package task

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.xchunk.org/talos/internal/opencode"
)

// fakeBackend stands in for OpenCode. Hooks are read under mu; tests set them
// before the behavior they affect.
type fakeBackend struct {
	mu          sync.Mutex
	nextSession int
	sessions    []opencode.CreateSessionRequest
	prompts     []string
	interrupts  []string
	ensureErr   error
	createErr   error
	promptErr   error
	blockCreate chan struct{}

	active      map[string]bool
	activeErr   error
	messages    map[string][]opencode.Message
	messagesErr error
	permissions map[string][]opencode.PermissionRequest
	children    map[string][]opencode.Session
	replyHook   func(string)
}

func (f *fakeBackend) Ensure(context.Context) (opencode.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return opencode.Info{Version: "2.0.32"}, f.ensureErr
}

func (f *fakeBackend) CreateSession(ctx context.Context, req opencode.CreateSessionRequest) (opencode.Session, error) {
	f.mu.Lock()
	block := f.blockCreate
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return opencode.Session{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return opencode.Session{}, f.createErr
	}
	f.nextSession++
	f.sessions = append(f.sessions, req)
	return opencode.Session{ID: fmt.Sprintf("ses_%d", f.nextSession)}, nil
}

func (f *fakeBackend) Prompt(_ context.Context, sessionID, text, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.promptErr != nil {
		return "", f.promptErr
	}
	f.prompts = append(f.prompts, sessionID+":"+text)
	return "msg", nil
}

func (f *fakeBackend) Interrupt(_ context.Context, sessionID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interrupts = append(f.interrupts, sessionID)
	return true, nil
}

func (f *fakeBackend) ReplyPermission(_ context.Context, session, request, decision, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replyHook != nil {
		f.replyHook(session + "/" + request + "/" + decision + "/" + message)
	}
	return nil
}

func (f *fakeBackend) ActiveSessions(context.Context) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active, f.activeErr
}

func (f *fakeBackend) RecentMessages(_ context.Context, sessionID string, _ int) ([]opencode.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.messages[sessionID], f.messagesErr
}

func (f *fakeBackend) Permissions(_ context.Context, sessionID string) ([]opencode.PermissionRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.permissions[sessionID], nil
}

func (f *fakeBackend) ChildSessions(_ context.Context, parentID string) ([]opencode.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.children[parentID], nil
}

func (f *fakeBackend) CancelForm(context.Context, string, string) error { return nil }

func (f *fakeBackend) Stream(ctx context.Context, _ opencode.StreamOptions, _ func(opencode.Event)) error {
	<-ctx.Done()
	return ctx.Err()
}

// harness runs a Manager with a fake backend on a real temporary repository.
type harness struct {
	t       *testing.T
	m       *Manager
	backend *fakeBackend
	store   *FileStore
	repo    string
	stop    func()
}

func newHarness(t *testing.T, mutate ...func(*Options)) *harness {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	state := t.TempDir()
	store, err := NewFileStore(filepath.Join(state, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{}
	opts := Options{
		Backend:           backend,
		Store:             store,
		Roots:             []string{repo},
		WorktreeDir:       filepath.Join(state, "worktrees"),
		MaxParallel:       3,
		Timeout:           time.Hour,
		MaxSteps:          100,
		PermissionTimeout: time.Hour,
	}
	for _, fn := range mutate {
		fn(&opts)
	}
	m := NewManager(opts)
	m.Adopt()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	h := &harness{t: t, m: m, backend: backend, store: store, repo: repo}
	var once sync.Once
	h.stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(h.stop)
	return h
}

func (h *harness) start() *Task {
	h.t.Helper()
	task, err := h.m.Start(h.t.Context(), StartRequest{Directory: h.repo, Prompt: "work"})
	if err != nil {
		h.t.Fatal(err)
	}
	return task
}

// eventually polls because dispatching a turn happens on its own goroutine.
func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) waitState(id string, want State) *Task {
	h.t.Helper()
	var last *Task
	h.eventually(fmt.Sprintf("task %s to reach %s", id, want), func() bool {
		var err error
		last, err = h.m.Get(id)
		return err == nil && last.State == want
	})
	return last
}

// waitPrompted waits for the prompt to be delivered, after which the turn is live.
func (h *harness) waitPrompted(id string) *Task {
	h.t.Helper()
	var last *Task
	h.eventually("prompt delivery", func() bool {
		last, _ = h.m.Get(id)
		return last != nil && last.Prompt == "" && last.SessionID != ""
	})
	return last
}

func TestStartCreatesWorktreeAndRunsPrompt(t *testing.T) {
	h := newHarness(t)
	created := h.start()
	if _, err := os.Stat(created.Worktree); err != nil {
		t.Fatal(err)
	}
	if created.Branch != "talos/"+created.ID || created.BaseCommit == "" {
		t.Errorf("task = %+v", created)
	}
	task := h.waitPrompted(created.ID)
	if task.State != StateRunning || task.Turn != 1 {
		t.Errorf("task = %+v", task)
	}
	h.backend.mu.Lock()
	defer h.backend.mu.Unlock()
	if h.backend.prompts[0] != task.SessionID+":work" {
		t.Errorf("prompts = %v", h.backend.prompts)
	}
	if got := h.backend.sessions[0].Location.Directory; got != created.Worktree {
		t.Errorf("session directory = %q", got)
	}
}

func TestSessionRulesPutDeniesAfterAllow(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.ShellDeny = []string{"git push*"} })
	rules := h.m.sessionRules()
	if rules[0].Effect != opencode.EffectAllow || rules[0].Action != "*" {
		t.Errorf("first rule = %+v", rules[0])
	}
	last := rules[len(rules)-1]
	if last != (opencode.PermissionRule{Action: "shell", Resource: "git push*", Effect: opencode.EffectDeny}) {
		t.Errorf("last rule = %+v", last)
	}
}

func TestDenyOverridesAskForTheSameCommand(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.ShellAsk = []string{"git *"}
		o.ShellDeny = []string{"git push*"}
	})
	rules := h.m.sessionRules()
	if ask, deny := rules[len(rules)-2], rules[len(rules)-1]; ask.Effect != opencode.EffectAsk || deny.Effect != opencode.EffectDeny {
		t.Errorf("ask = %+v, deny = %+v", ask, deny)
	}
}

func TestStartValidation(t *testing.T) {
	h := newHarness(t)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	notRepo := filepath.Join(h.repo, "..")
	tests := []struct {
		name string
		req  StartRequest
		want string
	}{
		{"empty prompt", StartRequest{Directory: h.repo, Prompt: " "}, "prompt"},
		{"relative", StartRequest{Directory: "x", Prompt: "p"}, "absolute"},
		{"missing", StartRequest{Directory: h.repo + "/nope", Prompt: "p"}, "resolve"},
		{"not a repository", StartRequest{Directory: outside, Prompt: "p"}, "not in a git repository"},
		{"parent of root", StartRequest{Directory: notRepo, Prompt: "p"}, ""},
		{"bad model", StartRequest{Directory: h.repo, Prompt: "p", Model: "nope"}, "provider/model"},
		{"bad ref", StartRequest{Directory: h.repo, Prompt: "p", BaseRef: "nope"}, "base"},
		{"negative", StartRequest{Directory: h.repo, Prompt: "p", MaxSteps: -1}, "negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.m.Start(t.Context(), tt.req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v", err)
			}
		})
	}
	if tasks := h.m.List(""); len(tasks) != 0 {
		t.Errorf("rejected requests left tasks: %v", tasks)
	}
}

func TestStartRejectsRepositoryOutsideRoots(t *testing.T) {
	h := newHarness(t)
	h.m.opts.Roots = []string{filepath.Join(h.repo, "sub")}
	sub := filepath.Join(h.repo, "sub")
	os.Mkdir(sub, 0o755)
	_, err := h.m.Start(t.Context(), StartRequest{Directory: sub, Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "allowed roots") {
		t.Errorf("an allowed subdirectory must not expose its repository: %v", err)
	}
}

func TestStartResolvesSymlinksBeforeChecking(t *testing.T) {
	h := newHarness(t)
	other := newHarness(t).repo
	link := filepath.Join(h.repo, "escape")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	_, err := h.m.Start(t.Context(), StartRequest{Directory: link, Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "allowed roots") {
		t.Errorf("err = %v", err)
	}
}

func TestSchedulerRespectsMaxParallel(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxParallel = 2 })
	first, second, third := h.start(), h.start(), h.start()
	h.waitPrompted(first.ID)
	h.waitPrompted(second.ID)
	if got, _ := h.m.Get(third.ID); got.State != StateQueued {
		t.Fatalf("third = %s", got.State)
	}
	h.m.update(first.ID, func(t *Task) bool {
		h.m.finishLocked(t, StateSucceeded)
		return true
	})
	h.waitPrompted(third.ID)
}

func TestBackendFailuresFailTheTask(t *testing.T) {
	tests := []struct {
		name string
		set  func(*fakeBackend)
		want string
	}{
		{"server down", func(f *fakeBackend) { f.ensureErr = errors.New("down") }, "not available"},
		{"create", func(f *fakeBackend) { f.createErr = errors.New("nope") }, "create session"},
		{"prompt", func(f *fakeBackend) { f.promptErr = errors.New("nope") }, "send prompt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.set(h.backend)
			task := h.waitState(h.start().ID, StateFailed)
			if !strings.Contains(task.Error, tt.want) {
				t.Errorf("error = %q", task.Error)
			}
			if _, err := os.Stat(task.Worktree); err != nil {
				t.Errorf("the worktree must survive: %v", err)
			}
		})
	}
}

func TestStateReachesDisk(t *testing.T) {
	h := newHarness(t)
	task := h.waitPrompted(h.start().ID)
	h.stop()
	stored, err := h.store.read(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SessionID != task.SessionID || stored.State != StateRunning {
		t.Errorf("stored = %+v", stored)
	}
}

// slowStore proves that a stuck disk cannot hold up state changes.
type slowStore struct {
	Store
	release chan struct{}
}

func (s *slowStore) Save(t *Task) error {
	<-s.release
	return s.Store.Save(t)
}

func TestSlowStoreDoesNotBlockUpdates(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(o *Options) { o.Store = &slowStore{Store: o.Store, release: release} })
	h.m.mu.Lock()
	h.m.tasks["a"] = &Task{ID: "a", State: StateRunning}
	h.m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			h.m.update("a", func(*Task) bool { return true })
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("updates blocked on the store")
	}
	close(release)
}

func TestWait(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.tasks["a"] = &Task{ID: "a", State: StateRunning}
	h.m.tasks["b"] = &Task{ID: "b", State: StateRunning}
	h.m.mu.Unlock()

	t.Run("timeout is not an error", func(t *testing.T) {
		tasks, err := h.m.Wait(t.Context(), []string{"a"}, 5*time.Millisecond)
		if err != nil || len(tasks) != 1 || tasks[0].State != StateRunning {
			t.Errorf("tasks = %v, err = %v", tasks, err)
		}
	})
	t.Run("unknown id", func(t *testing.T) {
		if _, err := h.m.Wait(t.Context(), []string{"nope"}, time.Second); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("honors ctx", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := h.m.Wait(ctx, []string{"a"}, time.Minute); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("wakes on change", func(t *testing.T) {
		go func() {
			time.Sleep(5 * time.Millisecond)
			h.m.update("b", func(t *Task) bool {
				h.m.finishLocked(t, StateSucceeded)
				return true
			})
		}()
		tasks, err := h.m.Wait(t.Context(), []string{"a", "b"}, time.Minute)
		if err != nil || tasks[1].State != StateSucceeded {
			t.Errorf("tasks = %v, err = %v", tasks, err)
		}
	})
	t.Run("finished returns at once", func(t *testing.T) {
		start := time.Now()
		if _, err := h.m.Wait(t.Context(), []string{"b"}, time.Minute); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > time.Second {
			t.Error("did not return at once")
		}
	})
}
