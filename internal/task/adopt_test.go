package task

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"go.xchunk.org/talos/internal/opencode"
)

// deadPID returns the PID of a process that has already exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// restarted runs a fresh manager over the given records, as after a crash.
func restarted(t *testing.T, records []*Task, mutate ...func(*fakeBackend)) *harness {
	t.Helper()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := store.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, func(o *Options) {
		o.Store = store
		for _, fn := range mutate {
			fn(o.Backend.(*fakeBackend))
		}
	})
	h.store = store
	return h
}

func TestAdoptTaskThatFinishedWhileDown(t *testing.T) {
	sent := time.Now().Add(-time.Minute)
	record := &Task{ID: "a", State: StateRunning, SessionID: "s", OwnerPID: deadPID(t), PromptSentAt: sent, TurnStartedAt: sent, Timeout: time.Hour}
	h := restarted(t, []*Task{record}, func(f *fakeBackend) {
		f.messages = map[string][]opencode.Message{"s": {idleMessage(opencode.OutcomeSucceeded, time.Now())}}
	})
	h.waitState("a", StateSucceeded)
	h.stop()
	stored, err := h.store.read("a")
	if err != nil {
		t.Fatal(err)
	}
	if stored.OwnerPID != os.Getpid() || stored.State != StateSucceeded {
		t.Errorf("stored = %+v", stored)
	}
}

func TestAdoptKeepsTaskStillRunning(t *testing.T) {
	sent := time.Now().Add(-time.Minute)
	record := &Task{ID: "a", State: StateRunning, SessionID: "s", OwnerPID: deadPID(t), PromptSentAt: sent, TurnStartedAt: sent, Timeout: time.Hour}
	h := restarted(t, []*Task{record}, func(f *fakeBackend) { f.active = map[string]bool{"s": true} })
	time.Sleep(20 * time.Millisecond)
	if got := h.get("a"); got.State != StateRunning {
		t.Errorf("state = %s", got.State)
	}
	h.m.handleEvent(event(opencode.EventExecutionSucceeded, map[string]any{"sessionID": "s"}))
	if got := h.get("a"); got.State != StateSucceeded {
		t.Errorf("state = %s", got.State)
	}
}

func TestAdoptRequeuesTaskThatNeverStarted(t *testing.T) {
	dead := deadPID(t)
	records := []*Task{
		{ID: "queued", State: StateQueued, Prompt: "p1", OwnerPID: dead, MaxSteps: 1},
		{ID: "promoted", State: StateRunning, Prompt: "p2", Turn: 1, OwnerPID: dead, MaxSteps: 1},
	}
	h := restarted(t, records)
	for _, id := range []string{"queued", "promoted"} {
		task := h.waitPrompted(id)
		if task.State != StateRunning || task.Turn != 1 {
			t.Errorf("%s: %+v", id, task)
		}
	}
}

func TestAdoptLeavesLiveOwnersAlone(t *testing.T) {
	// The test binary's parent is alive and is not this process.
	record := &Task{ID: "a", State: StateRunning, SessionID: "s", OwnerPID: os.Getppid()}
	h := restarted(t, []*Task{record})
	if _, err := h.m.Get("a"); err == nil {
		t.Error("adopted a task with a live owner")
	}
	h.stop()
	stored, err := h.store.read("a")
	if err != nil {
		t.Fatal(err)
	}
	if stored.OwnerPID != os.Getppid() {
		t.Errorf("owner = %d", stored.OwnerPID)
	}
}

func TestTwoInstancesCompetingAdoptOnce(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.Save(&Task{ID: "a", State: StateSucceeded, OwnerPID: deadPID(t)})

	// The winner's PID must look alive to the loser, so both use real live PIDs.
	first := NewManager(Options{Store: store, Backend: &fakeBackend{}, PID: os.Getpid()})
	second := NewManager(Options{Store: store, Backend: &fakeBackend{}, PID: os.Getppid()})
	defer first.cancel()
	defer second.cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		first.Adopt()
	}()
	second.Adopt()
	<-done

	_, firstErr := first.Get("a")
	_, secondErr := second.Get("a")
	if (firstErr == nil) == (secondErr == nil) {
		t.Errorf("first = %v, second = %v", firstErr, secondErr)
	}
}

func TestAdoptedFinishedTaskStaysAvailable(t *testing.T) {
	h := restarted(t, []*Task{{ID: "a", State: StateSucceeded, OwnerPID: deadPID(t)}})
	if got := h.get("a"); got.State != StateSucceeded {
		t.Errorf("state = %s", got.State)
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) || processAlive(deadPID(t)) || processAlive(0) {
		t.Error("wrong answer")
	}
}
