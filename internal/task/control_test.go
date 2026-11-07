package task

import (
	"strings"
	"testing"

	"go.xchunk.org/talos/internal/opencode"
)

func TestFollowUpOnFinishedTaskIsANewTurnInSameSession(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	h.m.handleEvent(event(opencode.EventStepStarted, map[string]any{"sessionID": task.SessionID}))
	h.m.handleEvent(event(opencode.EventExecutionFailed, map[string]any{"sessionID": task.SessionID}))
	if _, err := h.m.SendMessage(t.Context(), task.ID, "again"); err != nil {
		t.Fatal(err)
	}
	h.eventually("second prompt", func() bool {
		h.backend.mu.Lock()
		defer h.backend.mu.Unlock()
		return len(h.backend.prompts) == 2 && h.backend.prompts[1] == task.SessionID+":again"
	})
	got := h.waitPrompted(task.ID)
	if got.State != StateRunning || got.Turn != 2 || got.Steps != 0 || got.Error != "" {
		t.Errorf("task = %+v", got)
	}
	if len(h.backend.sessions) != 1 {
		t.Errorf("sessions = %d", len(h.backend.sessions))
	}
}

func TestFollowUpRespectsMaxParallel(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxParallel = 1 })
	first := h.live()
	h.m.handleEvent(event(opencode.EventExecutionSucceeded, map[string]any{"sessionID": first.SessionID}))
	second := h.live()
	got, err := h.m.SendMessage(t.Context(), first.ID, "again")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateQueued {
		t.Errorf("state = %s", got.State)
	}
	h.m.handleEvent(event(opencode.EventExecutionSucceeded, map[string]any{"sessionID": second.SessionID}))
	h.waitPrompted(first.ID)
}

func TestMessageOnRunningTaskSteers(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	got, err := h.m.SendMessage(t.Context(), task.ID, "steer me")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateRunning || got.Turn != 1 {
		t.Errorf("task = %+v", got)
	}
	h.backend.mu.Lock()
	defer h.backend.mu.Unlock()
	if h.backend.prompts[1] != task.SessionID+":steer me" {
		t.Errorf("prompts = %v", h.backend.prompts)
	}
}

func TestMessageOnQueuedTaskJoinsThePrompt(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxParallel = 1 })
	first := h.live()
	queued := h.start()
	if _, err := h.m.SendMessage(t.Context(), queued.ID, "more"); err != nil {
		t.Fatal(err)
	}
	h.m.handleEvent(event(opencode.EventExecutionSucceeded, map[string]any{"sessionID": first.SessionID}))
	got := h.waitPrompted(queued.ID)
	h.backend.mu.Lock()
	defer h.backend.mu.Unlock()
	if last := h.backend.prompts[len(h.backend.prompts)-1]; last != got.SessionID+":work\n\nmore" {
		t.Errorf("prompt = %q", last)
	}
}

func TestMessageWhileTurnStartsIsRefused(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	h := newHarness(t)
	h.backend.set(func(f *fakeBackend) { f.blockCreate = block })
	task := h.start()
	h.waitState(task.ID, StateRunning)
	if _, err := h.m.SendMessage(t.Context(), task.ID, "x"); err == nil || !strings.Contains(err.Error(), "starting") {
		t.Errorf("err = %v", err)
	}
}

func TestRespondPermissionRepliesThroughOwningSession(t *testing.T) {
	for _, tt := range []struct {
		allow bool
		want  string
	}{{true, opencode.DecisionOnce}, {false, opencode.DecisionReject}} {
		h := newHarness(t)
		task := h.live()
		var replies []string
		h.backend.set(func(f *fakeBackend) { f.replyHook = func(s string) { replies = append(replies, s) } })
		h.m.handleEvent(event(opencode.EventSessionCreated, map[string]any{"sessionID": "child", "parentID": task.SessionID}))
		h.m.handleEvent(event(opencode.EventPermissionAsked, map[string]any{"id": "p", "sessionID": "child"}))

		got, err := h.m.RespondPermission(t.Context(), task.ID, "p", tt.allow, "why")
		if err != nil {
			t.Fatal(err)
		}
		if got.State != StateRunning || len(got.Pending) != 0 {
			t.Errorf("task = %+v", got)
		}
		if want := "child/p/" + tt.want + "/why"; len(replies) != 1 || replies[0] != want {
			t.Errorf("replies = %v, want %s", replies, want)
		}
		if _, err := h.m.RespondPermission(t.Context(), task.ID, "p", true, ""); err == nil {
			t.Error("a repeated answer must be an error")
		}
	}
}

func TestAbortQueuedTaskEndsAtOnce(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxParallel = 1 })
	h.live()
	queued := h.start()
	got, err := h.m.Abort(queued.ID)
	if err != nil || got.State != StateAborted {
		t.Errorf("task = %+v, err = %v", got, err)
	}
}

func TestAbortDuringSetupStopsThePrompt(t *testing.T) {
	block := make(chan struct{})
	h := newHarness(t)
	h.backend.set(func(f *fakeBackend) { f.blockCreate = block })
	task := h.start()
	h.waitState(task.ID, StateRunning)
	got, _ := h.m.Abort(task.ID)
	if got.State != StateAborted {
		t.Errorf("state = %s", got.State)
	}
	close(block)
	h.stop()
	if len(h.backend.prompts) != 0 {
		t.Errorf("prompts = %v", h.backend.prompts)
	}
}

func TestAbortRunningTaskFollowsTheEvent(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	h.backend.set(func(f *fakeBackend) { f.active = map[string]bool{task.SessionID: true} })
	got, err := h.m.Abort(task.ID)
	if err != nil || got.State != StateRunning {
		t.Fatalf("task = %+v, err = %v", got, err)
	}
	h.eventually("interrupt", func() bool { return len(h.interrupts()) == 1 })
	h.m.handleEvent(event(opencode.EventExecutionInterrupted, map[string]any{"sessionID": task.SessionID, "reason": "user"}))
	if got := h.get(task.ID); got.State != StateAborted {
		t.Errorf("state = %s", got.State)
	}
	version := h.get(task.ID).Version
	if got, _ := h.m.Abort(task.ID); got.Version != version {
		t.Error("aborting a finished task must change nothing")
	}
}
