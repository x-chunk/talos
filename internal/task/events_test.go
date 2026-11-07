package task

import (
	"encoding/json"
	"strings"
	"testing"

	"go.xchunk.org/talos/internal/opencode"
)

func event(typ string, data map[string]any) opencode.Event {
	raw, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return opencode.Event{Type: typ, Data: raw}
}

// live starts a task and waits until its turn is underway.
func (h *harness) live() *Task {
	h.t.Helper()
	return h.waitPrompted(h.start().ID)
}

func (h *harness) get(id string) *Task {
	h.t.Helper()
	task, err := h.m.Get(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return task
}

func TestRootOutcomesEndTheTask(t *testing.T) {
	tests := []struct {
		name  string
		event opencode.Event
		want  State
		err   string
	}{
		{"succeeded", event(opencode.EventExecutionSucceeded, nil), StateSucceeded, ""},
		{"failed", event(opencode.EventExecutionFailed, map[string]any{
			"error": map[string]any{"type": "provider.no-route", "message": "no route"},
		}), StateFailed, "provider.no-route: no route"},
		{"user interrupt", event(opencode.EventExecutionInterrupted, map[string]any{"reason": "user"}), StateAborted, ""},
		{"other interrupt", event(opencode.EventExecutionInterrupted, map[string]any{"reason": "shutdown"}), StateFailed, "shutdown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			task := h.live()
			var data map[string]any
			json.Unmarshal(tt.event.Data, &data)
			if data == nil {
				data = map[string]any{}
			}
			data["sessionID"] = task.SessionID
			h.m.handleEvent(event(tt.event.Type, data))
			got := h.get(task.ID)
			if got.State != tt.want {
				t.Errorf("state = %s", got.State)
			}
			if !strings.Contains(got.Error, tt.err) {
				t.Errorf("error = %q", got.Error)
			}
		})
	}
}

func TestChildSessionOutcomesAreIgnored(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	h.m.handleEvent(event(opencode.EventSessionCreated, map[string]any{"sessionID": "child", "parentID": task.SessionID}))
	h.m.handleEvent(event(opencode.EventSessionCreated, map[string]any{"sessionID": "grandchild", "parentID": "child"}))
	h.m.handleEvent(event(opencode.EventExecutionFailed, map[string]any{"sessionID": "child"}))
	h.m.handleEvent(event(opencode.EventExecutionSucceeded, map[string]any{"sessionID": "grandchild"}))
	got := h.get(task.ID)
	if got.State != StateRunning || len(got.Children) != 2 {
		t.Errorf("task = %+v", got)
	}
}

func TestPermissionFlowIncludingChild(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	h.m.handleEvent(event(opencode.EventSessionCreated, map[string]any{"sessionID": "child", "parentID": task.SessionID}))

	asked := func(id, session string) {
		h.m.handleEvent(event(opencode.EventPermissionAsked, map[string]any{
			"id": id, "sessionID": session, "action": "shell", "resources": []string{"ls"},
		}))
	}
	asked("p1", task.SessionID)
	asked("p2", "child")
	asked("p2", "child") // A duplicate must not add a second entry.
	got := h.get(task.ID)
	if got.State != StateWaitingPermission || len(got.Pending) != 2 || got.Pending[1].SessionID != "child" {
		t.Fatalf("task = %+v", got)
	}

	h.m.handleEvent(event(opencode.EventPermissionReplied, map[string]any{"sessionID": task.SessionID, "requestID": "p1"}))
	if got := h.get(task.ID); got.State != StateWaitingPermission {
		t.Errorf("one left pending: state = %s", got.State)
	}
	h.m.handleEvent(event(opencode.EventPermissionReplied, map[string]any{"sessionID": "child", "requestID": "p2"}))
	if got := h.get(task.ID); got.State != StateRunning || got.Pending != nil && len(got.Pending) != 0 {
		t.Errorf("task = %+v", got)
	}
}

func TestStepLimitInterruptsAndFails(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxSteps = 2 })
	task := h.live()
	step := event(opencode.EventStepStarted, map[string]any{"sessionID": task.SessionID})
	h.m.handleEvent(step)
	h.m.handleEvent(step)
	if len(h.interrupts()) != 0 {
		t.Fatal("interrupted at the limit, not over it")
	}
	h.m.handleEvent(step)
	h.m.handleEvent(step)
	h.eventually("interrupt", func() bool { return len(h.interrupts()) == 1 })
	h.m.handleEvent(event(opencode.EventExecutionInterrupted, map[string]any{"sessionID": task.SessionID, "reason": "user"}))
	got := h.get(task.ID)
	if got.State != StateFailed || !strings.Contains(got.Error, "step limit") {
		t.Errorf("task = %+v", got)
	}
}

func (h *harness) interrupts() []string {
	h.backend.mu.Lock()
	defer h.backend.mu.Unlock()
	return append([]string(nil), h.backend.interrupts...)
}

func TestLateEventsCannotReviveFinishedTask(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	h.m.handleEvent(event(opencode.EventExecutionInterrupted, map[string]any{"sessionID": task.SessionID, "reason": "user"}))
	version := h.get(task.ID).Version
	h.m.handleEvent(event(opencode.EventExecutionSucceeded, map[string]any{"sessionID": task.SessionID}))
	h.m.handleEvent(event(opencode.EventPermissionAsked, map[string]any{"id": "p", "sessionID": task.SessionID}))
	got := h.get(task.ID)
	if got.State != StateAborted || got.Version != version {
		t.Errorf("task = %+v", got)
	}
}

func TestSuccessWinsOverPendingInterrupt(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	h.m.update(task.ID, func(tk *Task) bool {
		h.m.requestInterruptLocked(tk, IntentAbort)
		return true
	})
	h.m.handleEvent(event(opencode.EventExecutionSucceeded, map[string]any{"sessionID": task.SessionID}))
	if got := h.get(task.ID); got.State != StateSucceeded || got.Intent != IntentNone {
		t.Errorf("task = %+v", got)
	}
}

func TestDeniedActionsCount(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	for _, typ := range []string{opencode.ErrorPermissionRejected, "other"} {
		h.m.handleEvent(event(opencode.EventToolFailed, map[string]any{
			"sessionID": task.SessionID, "error": map[string]any{"type": typ},
		}))
	}
	if got := h.get(task.ID); got.DeniedActions != 1 {
		t.Errorf("denied = %d", got.DeniedActions)
	}
}

func TestUsageIsCumulativePerSession(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	h.m.handleEvent(event(opencode.EventSessionCreated, map[string]any{"sessionID": "child", "parentID": task.SessionID}))
	usage := func(session string, input int) {
		h.m.handleEvent(event(opencode.EventUsageUpdated, map[string]any{
			"sessionID": session, "cost": 0.5, "tokens": map[string]any{"input": input, "output": 1},
		}))
	}
	usage(task.SessionID, 100)
	usage(task.SessionID, 150) // A new total for the root, not an addition.
	usage("child", 10)
	got := h.get(task.ID)
	if got.InputTokens != 160 || got.OutputTokens != 2 || got.Cost != 1 {
		t.Errorf("task = %+v", got)
	}
}

func TestProgressShowsCurrentTool(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	h.m.handleEvent(event(opencode.EventToolInputStarted, map[string]any{"sessionID": task.SessionID, "name": "write"}))
	if got := h.get(task.ID); got.Progress != "running tool write" {
		t.Errorf("progress = %q", got.Progress)
	}
}

func TestFormFailsTheTask(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	h.m.handleEvent(event(opencode.EventFormCreated, map[string]any{
		"form": map[string]any{"id": "frm", "sessionID": task.SessionID},
	}))
	h.m.handleEvent(event(opencode.EventExecutionInterrupted, map[string]any{"sessionID": task.SessionID, "reason": "shutdown"}))
	got := h.get(task.ID)
	if got.State != StateFailed || !strings.Contains(got.Error, "form") {
		t.Errorf("task = %+v", got)
	}
}

func TestEventsForUnknownSessionsAreIgnored(t *testing.T) {
	h := newHarness(t)
	task := h.live()
	version := h.get(task.ID).Version
	h.m.handleEvent(event(opencode.EventExecutionSucceeded, map[string]any{"sessionID": "other"}))
	h.m.handleEvent(opencode.Event{Type: opencode.EventExecutionFailed, Data: []byte(`not json`)})
	h.m.handleEvent(opencode.Event{Type: "session.text.delta", Data: []byte(`{}`)})
	if got := h.get(task.ID); got.Version != version {
		t.Errorf("task changed: %+v", got)
	}
}
