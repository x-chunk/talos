package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestDefaults(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	cfg, err := Load(env(map[string]string{"XDG_STATE_HOME": "/state"}), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Autostart || cfg.MaxParallel != 3 || cfg.MaxSteps != 200 || cfg.ResultLimit != 8000 {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.TaskTimeout != 30*time.Minute || cfg.PermissionTimeout != 10*time.Minute {
		t.Errorf("cfg = %+v", cfg)
	}
	if !slices.Equal(cfg.AllowedRoots, []string{dir}) || cfg.StateDir != "/state/talos" {
		t.Errorf("roots = %v, state = %s", cfg.AllowedRoots, cfg.StateDir)
	}
	if !slices.Contains(cfg.ShellDeny, "git push*") {
		t.Errorf("shell deny = %v", cfg.ShellDeny)
	}
}

func TestOverrides(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env(map[string]string{
		"TALOS_AUTOSTART":     "false",
		"TALOS_MAX_PARALLEL":  "5",
		"TALOS_TASK_TIMEOUT":  "1h",
		"TALOS_DEFAULT_MODEL": "opencode/x",
		"TALOS_SHELL_DENY":    "a *, b *",
		"TALOS_SHELL_ASK":     "c *",
		"TALOS_ALLOWED_ROOTS": link,
		"TALOS_STATE_DIR":     "/s",
	}), "/unused")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Autostart || cfg.MaxParallel != 5 || cfg.TaskTimeout != time.Hour || cfg.StateDir != "/s" {
		t.Errorf("cfg = %+v", cfg)
	}
	if !slices.Equal(cfg.ShellDeny, []string{"a *", "b *"}) {
		t.Errorf("shell deny = %v", cfg.ShellDeny)
	}
	if !slices.Equal(cfg.ShellAsk, []string{"c *"}) {
		t.Errorf("shell ask = %v", cfg.ShellAsk)
	}
	if !slices.Equal(cfg.AllowedRoots, []string{dir}) {
		t.Errorf("roots = %v, want the resolved %s", cfg.AllowedRoots, dir)
	}
}

func TestReportsEveryInvalidValue(t *testing.T) {
	_, err := Load(env(map[string]string{
		"TALOS_MAX_PARALLEL":  "0",
		"TALOS_TASK_TIMEOUT":  "soon",
		"TALOS_DEFAULT_MODEL": "noslash",
		"TALOS_ALLOWED_ROOTS": "/does/not/exist",
		"TALOS_STATE_DIR":     "/s",
	}), "/")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, name := range []string{"TALOS_MAX_PARALLEL", "TALOS_TASK_TIMEOUT", "TALOS_DEFAULT_MODEL", "TALOS_ALLOWED_ROOTS"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error lacks %s: %v", name, err)
		}
	}
}
