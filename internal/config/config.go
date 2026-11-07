// Package config reads Talos settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultShellDeny names shell commands that are risky in any language. It
// guards against accidents only: OpenCode matches patterns against the command
// line, so `bash -c` gets around it. Relative `rm -rf` stays allowed because
// build tools and agents use it routinely.
var DefaultShellDeny = []string{
	"sudo *", "su *", "doas *",
	"git push*", "git remote*", "git worktree*", "git config --global*",
	"rm -rf /*", "rm -rf ~*",
	"curl *| sh*", "curl *| bash*", "wget *| sh*", "wget *| bash*",
	"mkfs*", "dd *of=/dev/*", "shutdown*", "reboot*",
}

// Config holds the settings of one Talos instance.
type Config struct {
	OpenCodeURL       string
	OpenCodePassword  string
	Autostart         bool
	AllowedRoots      []string
	MaxParallel       int
	DefaultModel      string
	TaskTimeout       time.Duration
	MaxSteps          int
	PermissionTimeout time.Duration
	ResultLimit       int
	ShellDeny         []string
	StateDir          string
}

// Load builds a Config from getenv. workDir is the launch directory, the default
// allowed root. All invalid values are reported together, so a misconfigured
// server can be fixed in one pass.
func Load(getenv func(string) string, workDir string) (Config, error) {
	cfg := Config{
		OpenCodeURL:      getenv("TALOS_OPENCODE_URL"),
		OpenCodePassword: getenv("TALOS_OPENCODE_PASSWORD"),
		DefaultModel:     getenv("TALOS_DEFAULT_MODEL"),
		ShellDeny:        DefaultShellDeny,
	}
	var errs []error
	cfg.Autostart = parse(getenv, "TALOS_AUTOSTART", true, strconv.ParseBool, &errs)
	cfg.MaxParallel = parsePositive(getenv, "TALOS_MAX_PARALLEL", 3, &errs)
	cfg.MaxSteps = parsePositive(getenv, "TALOS_MAX_STEPS", 200, &errs)
	cfg.ResultLimit = parsePositive(getenv, "TALOS_RESULT_LIMIT", 8000, &errs)
	cfg.TaskTimeout = parseDuration(getenv, "TALOS_TASK_TIMEOUT", 30*time.Minute, &errs)
	cfg.PermissionTimeout = parseDuration(getenv, "TALOS_PERMISSION_TIMEOUT", 10*time.Minute, &errs)

	if m := cfg.DefaultModel; m != "" {
		if provider, model, ok := strings.Cut(m, "/"); !ok || provider == "" || model == "" {
			errs = append(errs, fmt.Errorf("TALOS_DEFAULT_MODEL: %q is not provider/model", m))
		}
	}
	if raw, ok := lookup(getenv, "TALOS_SHELL_DENY"); ok {
		cfg.ShellDeny = splitTrim(raw, ",")
	}

	roots := splitTrim(getenv("TALOS_ALLOWED_ROOTS"), ":")
	if len(roots) == 0 {
		roots = []string{workDir}
	}
	for _, root := range roots {
		resolved, err := resolveRoot(root)
		if err != nil {
			errs = append(errs, fmt.Errorf("TALOS_ALLOWED_ROOTS: %w", err))
			continue
		}
		cfg.AllowedRoots = append(cfg.AllowedRoots, resolved)
	}

	cfg.StateDir = getenv("TALOS_STATE_DIR")
	if cfg.StateDir == "" {
		state := getenv("XDG_STATE_HOME")
		if state == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				errs = append(errs, fmt.Errorf("locate state directory: %w", err))
			}
			state = filepath.Join(home, ".local", "state")
		}
		cfg.StateDir = filepath.Join(state, "talos")
	}
	return cfg, errors.Join(errs...)
}

// resolveRoot resolves symlinks now because directories in requests are resolved
// before being compared, and an unresolved root would never match them.
func resolveRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func lookup(getenv func(string) string, name string) (string, bool) {
	value := getenv(name)
	return value, value != ""
}

func parse[T any](getenv func(string) string, name string, fallback T, convert func(string) (T, error), errs *[]error) T {
	raw, ok := lookup(getenv, name)
	if !ok {
		return fallback
	}
	value, err := convert(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: invalid value %q", name, raw))
		return fallback
	}
	return value
}

func parsePositive(getenv func(string) string, name string, fallback int, errs *[]error) int {
	return parse(getenv, name, fallback, func(raw string) (int, error) {
		n, err := strconv.Atoi(raw)
		if err == nil && n <= 0 {
			return 0, errors.New("must be positive")
		}
		return n, err
	}, errs)
}

func parseDuration(getenv func(string) string, name string, fallback time.Duration, errs *[]error) time.Duration {
	return parse(getenv, name, fallback, func(raw string) (time.Duration, error) {
		d, err := time.ParseDuration(raw)
		if err == nil && d <= 0 {
			return 0, errors.New("must be positive")
		}
		return d, err
	}, errs)
}

func splitTrim(raw, sep string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, sep) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
