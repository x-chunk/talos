package task

import (
	"testing"
	"time"
)

func TestTransitions(t *testing.T) {
	tests := []struct {
		from, to State
		ok       bool
	}{
		{StateQueued, StateRunning, true},
		{StateQueued, StateFailed, true},
		{StateQueued, StateAborted, true},
		{StateQueued, StateSucceeded, false},
		{StateQueued, StateWaitingPermission, false},
		{StateRunning, StateWaitingPermission, true},
		{StateRunning, StateSucceeded, true},
		{StateRunning, StateTimedOut, true},
		{StateRunning, StateQueued, false},
		{StateWaitingPermission, StateRunning, true},
		{StateWaitingPermission, StateAborted, true},
		{StateAborted, StateSucceeded, false},
		{StateSucceeded, StateFailed, false},
		{StateSucceeded, StateRunning, false},
		{StateSucceeded, StateQueued, true},
		{StateTimedOut, StateQueued, true},
	}
	for _, tt := range tests {
		task := &Task{State: tt.from}
		err := task.transition(tt.to, time.Now())
		if (err == nil) != tt.ok {
			t.Errorf("%s -> %s: err = %v", tt.from, tt.to, err)
		}
		if err != nil && task.State != tt.from {
			t.Errorf("%s -> %s: state changed to %s", tt.from, tt.to, task.State)
		}
	}
}

func TestFinishingClearsPendingAndIntent(t *testing.T) {
	now := time.Unix(100, 0)
	task := &Task{State: StateRunning, Pending: []PendingPermission{{ID: "p"}}, Intent: IntentTimeout}
	if err := task.transition(StateTimedOut, now); err != nil {
		t.Fatal(err)
	}
	if task.Pending != nil || task.Intent != IntentNone || !task.FinishedAt.Equal(now) {
		t.Errorf("task = %+v", task)
	}
	if err := task.transition(StateQueued, now); err != nil {
		t.Fatal(err)
	}
	if !task.FinishedAt.IsZero() {
		t.Error("re-queuing must clear FinishedAt")
	}
}

func TestCloneSharesNothing(t *testing.T) {
	orig := &Task{Children: []string{"c"}, Pending: []PendingPermission{{Resources: []string{"r"}}}}
	c := orig.Clone()
	c.Children[0] = "x"
	c.Pending[0].Resources[0] = "x"
	if orig.Children[0] != "c" || orig.Pending[0].Resources[0] != "r" {
		t.Errorf("orig = %+v", orig)
	}
}

func TestOwnsSession(t *testing.T) {
	task := &Task{SessionID: "root", Children: []string{"child"}}
	for id, want := range map[string]bool{"root": true, "child": true, "other": false, "": false} {
		if got := task.ownsSession(id); got != want {
			t.Errorf("ownsSession(%q) = %v", id, got)
		}
	}
	if (&Task{}).ownsSession("") {
		t.Error("a task without a session owns nothing")
	}
}

func TestNewIDIsHex(t *testing.T) {
	a, _ := newID()
	b, _ := newID()
	if len(a) != 8 || a == b {
		t.Errorf("a = %q, b = %q", a, b)
	}
}
