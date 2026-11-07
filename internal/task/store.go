package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Store persists tasks. Implementations must be safe for concurrent use.
type Store interface {
	// Save durably replaces the task's record.
	Save(t *Task) error
	// Delete removes the record. A missing record is not an error.
	Delete(id string) error
	// Load returns all records. A record that cannot be read is reported through
	// the returned error but does not hide the others.
	Load() ([]*Task, error)
	// Claim atomically takes ownership of the task if decide approves its current
	// record, returning the updated task, or nil if decide declined. It keeps two
	// instances from adopting one task.
	Claim(id string, decide func(*Task) bool, ownerPID int) (*Task, error)
}

// FileStore keeps one JSON file per task. Each Claude Code window runs its own
// Talos, and separate files mean instances cannot lose each other's updates.
type FileStore struct {
	dir string
}

// NewFileStore returns a store rooted at dir, creating it if needed. The mode is
// private because task files hold prompts.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create task directory: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

func (s *FileStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// Save writes through a synced temporary file and a rename, so a crash leaves
// either the old record or the new one, never a torn file.
func (s *FileStore) Save(t *Task) error {
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("encode task %s: %w", t.ID, err)
	}
	f, err := os.CreateTemp(s.dir, t.ID+".tmp-*")
	if err != nil {
		return fmt.Errorf("save task %s: %w", t.ID, err)
	}
	defer os.Remove(f.Name())
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), s.path(t.ID))
	}
	if err != nil {
		return fmt.Errorf("save task %s: %w", t.ID, err)
	}
	return nil
}

// Delete implements Store.
func (s *FileStore) Delete(id string) error {
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete task %s: %w", id, err)
	}
	return nil
}

// Load implements Store.
func (s *FileStore) Load() ([]*Task, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	var tasks []*Task
	var errs []error
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok {
			continue
		}
		t, err := s.read(id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks, errors.Join(errs...)
}

func (s *FileStore) read(id string) (*Task, error) {
	raw, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, fmt.Errorf("read task %s: %w", id, err)
	}
	var t Task
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("decode task %s: %w", id, err)
	}
	return &t, nil
}

// Claim implements Store under an advisory lock on the directory. The kernel
// drops the lock if the process dies, so a crash cannot wedge later instances.
func (s *FileStore) Claim(id string, decide func(*Task) bool, ownerPID int) (*Task, error) {
	lock, err := os.OpenFile(filepath.Join(s.dir, ".claim.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open claim lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("take claim lock: %w", err)
	}
	// Re-read under the lock: the record the caller saw may already be claimed.
	t, err := s.read(id)
	if err != nil {
		return nil, err
	}
	if !decide(t) {
		return nil, nil
	}
	t.OwnerPID = ownerPID
	if err := s.Save(t); err != nil {
		return nil, err
	}
	return t, nil
}
