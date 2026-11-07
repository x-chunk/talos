package task

import (
	"strings"
	"sync"
	"testing"
	"time"

	"go.xchunk.org/talos/internal/opencode"
)

// fakeClock lets watchdog tests jump past deadlines without waiting.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func clocked(t *testing.T, mutate ...func(*Options)) (*harness, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	h := newHarness(t, append([]func(*Options){func(o *Options) { o.Now = clock.Now }}, mutate...)...)
	return h, clock
}

// keepActive makes reconciliation see the session as still running, as a real
// server does while it works through an interrupt.
func (h *harness) keepActive(sessionID string) {
	h.backend.set(func(f *fakeBackend) { f.active = map[string]bool{sessionID: true} })
}

func TestTimeoutInterruptsThenEndsAsTimedOut(t *testing.T) {
	h, clock := clocked(t, func(o *Options) { o.Timeout = time.Minute })
	task := h.live()
	h.keepActive(task.SessionID)

	clock.Advance(59 * time.Second)
	h.m.checkLimits()
	if len(h.interrupts()) != 0 {
		t.Fatal("interrupted early")
	}
	clock.Advance(2 * time.Second)
	h.m.checkLimits()
	h.m.checkLimits() // A second pass must not interrupt again.
	h.eventually("interrupt", func() bool { return len(h.interrupts()) == 1 })
	if got := h.get(task.ID); got.State != StateRunning {
		t.Errorf("the task ends only when the interruption is reported: %s", got.State)
	}
	h.m.handleEvent(event(opencode.EventExecutionInterrupted, map[string]any{"sessionID": task.SessionID, "reason": "user"}))
	got := h.get(task.ID)
	if got.State != StateTimedOut || !strings.Contains(got.Error, "timed out") {
		t.Errorf("task = %+v", got)
	}
}

func TestPerTaskTimeoutOverridesDefault(t *testing.T) {
	h, clock := clocked(t)
	task, err := h.m.Start(t.Context(), StartRequest{Directory: h.repo, Prompt: "p", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	h.waitPrompted(task.ID)
	clock.Advance(2 * time.Second)
	h.m.checkLimits()
	h.eventually("interrupt", func() bool { return len(h.interrupts()) == 1 })
}

func TestPermissionTimeoutAborts(t *testing.T) {
	h, clock := clocked(t, func(o *Options) { o.PermissionTimeout = time.Minute })
	task := h.live()
	h.keepActive(task.SessionID)
	h.m.handleEvent(event(opencode.EventPermissionAsked, map[string]any{"id": "p", "sessionID": task.SessionID}))
	clock.Advance(2 * time.Minute)
	h.m.checkLimits()
	h.eventually("interrupt", func() bool { return len(h.interrupts()) == 1 })
	h.m.handleEvent(event(opencode.EventExecutionInterrupted, map[string]any{"sessionID": task.SessionID, "reason": "user"}))
	got := h.get(task.ID)
	if got.State != StateAborted || !strings.Contains(got.Error, "permission") {
		t.Errorf("task = %+v", got)
	}
}

func TestUnansweredInterruptEndsTaskAfterGrace(t *testing.T) {
	h, clock := clocked(t, func(o *Options) { o.Timeout = time.Minute })
	task := h.live()
	h.keepActive(task.SessionID)
	clock.Advance(2 * time.Minute)
	h.m.checkLimits()
	clock.Advance(interruptGrace - time.Second)
	h.m.checkLimits()
	if got := h.get(task.ID); got.State != StateRunning {
		t.Fatalf("ended within the grace period: %s", got.State)
	}
	clock.Advance(2 * time.Second)
	h.m.checkLimits()
	if got := h.get(task.ID); got.State != StateTimedOut {
		t.Errorf("state = %s", got.State)
	}
}

func TestTimedOutTaskFreesItsSlot(t *testing.T) {
	h, clock := clocked(t, func(o *Options) {
		o.Timeout = time.Minute
		o.MaxParallel = 1
	})
	first := h.live()
	second := h.start()
	clock.Advance(2 * time.Minute)
	h.m.checkLimits()
	h.m.handleEvent(event(opencode.EventExecutionInterrupted, map[string]any{"sessionID": first.SessionID, "reason": "user"}))
	h.waitPrompted(second.ID)
}
