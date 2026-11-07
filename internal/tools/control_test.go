package tools

import (
	"testing"

	"go.xchunk.org/argon/argontest"
	"go.xchunk.org/talos/internal/opencode"
)

func TestRespondPermissionTool(t *testing.T) {
	f := newFixture(t)
	started := f.start()
	f.backend.emit(t, opencode.EventPermissionAsked, map[string]any{"id": "per_1", "sessionID": "ses_1"})
	args := map[string]any{"task_id": started.TaskID, "request_id": "per_1", "decision": "maybe"}
	if _, err := argontest.Call[Status](t, f.s, "respond_permission", args); err == nil {
		t.Error("an unknown decision must be rejected")
	}
	args["decision"] = "deny"
	status, err := argontest.Call[Status](t, f.s, "respond_permission", args)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "running" || f.backend.replies[0] != "ses_1/per_1/reject" {
		t.Errorf("status = %+v, replies = %v", status, f.backend.replies)
	}
}

func TestSendMessageTool(t *testing.T) {
	f := newFixture(t)
	started := f.start()
	if _, err := argontest.Call[Status](t, f.s, "send_message", map[string]any{"task_id": started.TaskID, "text": "more"}); err != nil {
		t.Fatal(err)
	}
	f.backend.mu.Lock()
	defer f.backend.mu.Unlock()
	if last := f.backend.prompts[len(f.backend.prompts)-1]; last != opencode.DeliverySteer+":more" {
		t.Errorf("prompt = %q", last)
	}
}

func TestAbortToolReportsFinalState(t *testing.T) {
	f := newFixture(t)
	started := f.start()
	f.backend.emit(t, opencode.EventSessionCreated, map[string]any{"sessionID": "child", "parentID": "ses_1"})
	go func() {
		eventually(t, func() bool {
			tk, _ := f.m.Get(started.TaskID)
			return tk.Intent != ""
		})
		f.backend.emit(t, opencode.EventExecutionInterrupted, map[string]any{"sessionID": "ses_1", "reason": "user"})
	}()
	status, err := argontest.Call[Status](t, f.s, "abort_task", map[string]any{"task_id": started.TaskID})
	if err != nil || status.State != "aborted" {
		t.Errorf("status = %+v, err = %v", status, err)
	}
}
