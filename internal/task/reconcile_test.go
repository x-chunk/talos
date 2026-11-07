package task

import (
	"errors"
	"testing"
	"time"

	"go.xchunk.org/talos/internal/opencode"
)

func idleMessage(outcome string, created time.Time) opencode.Message {
	m := opencode.Message{Type: opencode.MessageIdle, Outcome: outcome}
	m.Time.Created = created.UnixMilli()
	return m
}

func (f *fakeBackend) set(fn func(*fakeBackend)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func TestReconcileAppliesMissedOutcome(t *testing.T) {
	tests := []struct {
		outcome string
		want    State
	}{
		{opencode.OutcomeSucceeded, StateSucceeded},
		{opencode.OutcomeFailed, StateFailed},
		{opencode.OutcomeInterrupted, StateAborted},
	}
	for _, tt := range tests {
		t.Run(tt.outcome, func(t *testing.T) {
			h, clock := clocked(t)
			task := h.live()
			h.backend.set(func(f *fakeBackend) {
				f.messages = map[string][]opencode.Message{task.SessionID: {idleMessage(tt.outcome, clock.Now())}}
			})
			h.m.reconcile(t.Context())
			if got := h.get(task.ID); got.State != tt.want {
				t.Errorf("state = %s", got.State)
			}
		})
	}
}

func TestReconcileFollowsIntentForMissedInterruption(t *testing.T) {
	h, clock := clocked(t)
	task := h.live()
	h.m.update(task.ID, func(tk *Task) bool {
		tk.Intent = IntentTimeout
		return true
	})
	h.backend.set(func(f *fakeBackend) {
		f.messages = map[string][]opencode.Message{task.SessionID: {idleMessage(opencode.OutcomeInterrupted, clock.Now())}}
	})
	h.m.reconcile(t.Context())
	if got := h.get(task.ID); got.State != StateTimedOut {
		t.Errorf("state = %s", got.State)
	}
}

func TestReconcileIgnoresIdleFromEarlierTurn(t *testing.T) {
	h, clock := clocked(t)
	task := h.live()
	h.backend.set(func(f *fakeBackend) {
		f.messages = map[string][]opencode.Message{
			task.SessionID: {idleMessage(opencode.OutcomeSucceeded, clock.Now().Add(-time.Minute))},
		}
	})
	h.m.reconcile(t.Context())
	if got := h.get(task.ID); got.State != StateRunning {
		t.Errorf("within grace: state = %s", got.State)
	}
	clock.Advance(reconcileGrace + time.Second)
	h.m.reconcile(t.Context())
	got := h.get(task.ID)
	if got.State != StateFailed || got.Error != "session lost" {
		t.Errorf("after grace: task = %+v", got)
	}
}

func TestReconcileFailsSessionThatIsGone(t *testing.T) {
	h, _ := clocked(t)
	task := h.live()
	h.backend.set(func(f *fakeBackend) { f.messagesErr = &opencode.APIError{Status: 404} })
	h.m.reconcile(t.Context())
	if got := h.get(task.ID); got.State != StateFailed {
		t.Errorf("state = %s", got.State)
	}
}

func TestReconcileLeavesTasksAloneWhenServerFails(t *testing.T) {
	failures := map[string]func(*fakeBackend){
		"ensure":   func(f *fakeBackend) { f.ensureErr = errors.New("down") },
		"active":   func(f *fakeBackend) { f.activeErr = errors.New("down") },
		"messages": func(f *fakeBackend) { f.messagesErr = errors.New("down") },
	}
	for name, fail := range failures {
		t.Run(name, func(t *testing.T) {
			h, clock := clocked(t)
			task := h.live()
			h.backend.set(fail)
			clock.Advance(time.Hour)
			h.m.reconcile(t.Context())
			if got := h.get(task.ID); got.State != StateRunning {
				t.Errorf("state = %s", got.State)
			}
		})
	}
}

func TestReconcileRecoversLostPermissionFromLostChild(t *testing.T) {
	h, _ := clocked(t)
	task := h.live()
	h.backend.set(func(f *fakeBackend) {
		f.active = map[string]bool{task.SessionID: true}
		f.children = map[string][]opencode.Session{task.SessionID: {{ID: "child"}}, "child": {{ID: "grandchild"}}}
		f.permissions = map[string][]opencode.PermissionRequest{
			"grandchild": {{ID: "p", SessionID: "grandchild", Action: "shell"}},
		}
	})
	h.m.reconcile(t.Context())
	got := h.get(task.ID)
	if got.State != StateWaitingPermission || got.Pending[0].SessionID != "grandchild" || len(got.Children) != 2 {
		t.Fatalf("task = %+v", got)
	}

	// The reply's event is lost too: the request is simply no longer listed.
	h.backend.set(func(f *fakeBackend) { f.permissions = nil })
	h.m.reconcile(t.Context())
	if got := h.get(task.ID); got.State != StateRunning {
		t.Errorf("state = %s", got.State)
	}
}

func TestReconcileRunsOnConnect(t *testing.T) {
	h, clock := clocked(t)
	task := h.live()
	h.backend.set(func(f *fakeBackend) {
		f.messages = map[string][]opencode.Message{task.SessionID: {idleMessage(opencode.OutcomeSucceeded, clock.Now())}}
	})
	h.m.requestReconcile()
	h.waitState(task.ID, StateSucceeded)
}

func TestReconcileSkipsTurnsNotYetSent(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	h, clock := clocked(t)
	h.backend.set(func(f *fakeBackend) { f.blockCreate = block })
	task := h.start()
	clock.Advance(time.Hour)
	h.m.reconcile(t.Context())
	if got := h.get(task.ID); got.State.Finished() {
		t.Errorf("state = %s", got.State)
	}
}

func TestReconcileLostSessionFollowsIntent(t *testing.T) {
	h, clock := clocked(t)
	task := h.live()
	h.m.update(task.ID, func(tk *Task) bool {
		tk.Intent = IntentTimeout
		return true
	})
	clock.Advance(reconcileGrace + time.Second)
	h.m.reconcile(t.Context())
	if got := h.get(task.ID); got.State != StateTimedOut {
		t.Errorf("state = %s", got.State)
	}
}
