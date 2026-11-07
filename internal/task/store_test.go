package task

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newStore(t *testing.T) *FileStore {
	t.Helper()
	s, err := NewFileStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := newStore(t)
	created := time.Unix(100, 0).UTC()
	in := &Task{
		ID: "a", State: StateWaitingPermission, Timeout: time.Minute, CreatedAt: created,
		Pending: []PendingPermission{{ID: "p", SessionID: "c", Resources: []string{"ls"}}},
	}
	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}
	in.State = StateRunning
	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.Load()
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks = %v, err = %v", tasks, err)
	}
	got := tasks[0]
	if got.State != StateRunning || got.Timeout != time.Minute || !got.CreatedAt.Equal(created) || got.Pending[0].SessionID != "c" {
		t.Errorf("got = %+v", got)
	}
	entries, _ := os.ReadDir(s.dir)
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestFilesArePrivate(t *testing.T) {
	s := newStore(t)
	s.Save(&Task{ID: "a"})
	info, err := os.Stat(s.path("a"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
}

func TestLoadSurvivesCorruptRecord(t *testing.T) {
	s := newStore(t)
	s.Save(&Task{ID: "good"})
	os.WriteFile(s.path("bad"), []byte("{"), 0o600)
	tasks, err := s.Load()
	if err == nil {
		t.Error("want the corrupt record reported")
	}
	if len(tasks) != 1 || tasks[0].ID != "good" {
		t.Errorf("tasks = %v", tasks)
	}
}

func TestDeleteMissingIsFine(t *testing.T) {
	s := newStore(t)
	if err := s.Delete("nope"); err != nil {
		t.Error(err)
	}
}

func TestClaimHasOneWinner(t *testing.T) {
	s := newStore(t)
	s.Save(&Task{ID: "a", OwnerPID: 1})
	var wins atomic.Int32
	var wg sync.WaitGroup
	for pid := 100; pid < 120; pid++ {
		wg.Go(func() {
			got, err := s.Claim("a", func(cur *Task) bool { return cur.OwnerPID == 1 }, pid)
			if err != nil {
				t.Error(err)
			}
			if got != nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("wins = %d", wins.Load())
	}
}
