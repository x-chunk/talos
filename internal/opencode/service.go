package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Endpoint sources reported by Locator.
const (
	SourceExplicit = "explicit"
	SourceService  = "service"
)

// ErrServiceNotRunning means the managed service has no registration file.
var ErrServiceNotRunning = errors.New("opencode: service is not running")

// Locator finds the server to talk to: an explicitly configured one, or else
// the user's managed service.
//
// It reads the service's registration file rather than asking the CLI, because
// ordinary CLI commands start the service as a side effect and Find must not.
type Locator struct {
	// URL and Password, when URL is set, select an explicit server. Find then
	// never looks at the service, and Start does not start it.
	URL, Password string
	// Binary is the opencode executable. Default "opencode".
	Binary string
	// StateHome overrides $XDG_STATE_HOME, for tests.
	StateHome string
}

// Source says where Find gets its endpoint.
func (l Locator) Source() string {
	if l.URL != "" {
		return SourceExplicit
	}
	return SourceService
}

// Find returns the endpoint without starting anything. The server may still be
// down: a stale registration survives a crash, so callers confirm with Info.
// It returns ErrServiceNotRunning if the service is not registered.
func (l Locator) Find() (Endpoint, error) {
	if l.URL != "" {
		return Endpoint{URL: l.URL, Password: l.Password}, nil
	}
	path, err := l.registrationPath()
	if err != nil {
		return Endpoint{}, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Endpoint{}, ErrServiceNotRunning
	}
	if err != nil {
		return Endpoint{}, fmt.Errorf("opencode: read service registration: %w", err)
	}
	var registration struct {
		URL      string `json:"url"`
		Password string `json:"password"`
	}
	// The decode error is dropped on purpose: encoding/json can quote input,
	// and this file holds the password.
	if json.Unmarshal(raw, &registration) != nil || registration.URL == "" {
		return Endpoint{}, fmt.Errorf("opencode: malformed service registration %s", path)
	}
	return Endpoint{URL: registration.URL, Password: registration.Password}, nil
}

// Start makes sure the managed service is running and returns its endpoint.
// The service outlives Talos, so tasks survive an orchestrator restart. Starting
// a running service is a cheap no-op. With an explicit server it only returns
// that server's endpoint.
func (l Locator) Start(ctx context.Context) (Endpoint, error) {
	if l.URL != "" {
		return l.Find()
	}
	binary := l.Binary
	if binary == "" {
		binary = "opencode"
	}
	cmd := exec.CommandContext(ctx, binary, "service", "start")
	if l.StateHome != "" {
		cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+l.StateHome)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return Endpoint{}, fmt.Errorf("opencode: service start: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return l.Find()
}

func (l Locator) registrationPath() (string, error) {
	state := l.StateHome
	if state == "" {
		state = os.Getenv("XDG_STATE_HOME")
	}
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("opencode: locate state directory: %w", err)
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "opencode", "service.json"), nil
}
