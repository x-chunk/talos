package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const registration = `{"id":"x","version":"2.0.32","url":"http://127.0.0.1:9","pid":1,"password":"pw"}`

func writeRegistration(t *testing.T, state, content string) {
	t.Helper()
	dir := filepath.Join(state, "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "service.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFindExplicitIgnoresService(t *testing.T) {
	l := Locator{URL: "http://a", Password: "b", StateHome: t.TempDir()}
	ep, err := l.Find()
	if err != nil {
		t.Fatal(err)
	}
	if ep != (Endpoint{URL: "http://a", Password: "b"}) || l.Source() != SourceExplicit {
		t.Errorf("ep = %+v, source = %s", ep, l.Source())
	}
}

func TestFindReadsRegistration(t *testing.T) {
	state := t.TempDir()
	writeRegistration(t, state, registration)
	ep, err := Locator{StateHome: state}.Find()
	if err != nil {
		t.Fatal(err)
	}
	if ep != (Endpoint{URL: "http://127.0.0.1:9", Password: "pw"}) {
		t.Errorf("ep = %+v", ep)
	}
}

func TestFindWithoutRegistration(t *testing.T) {
	_, err := Locator{StateHome: t.TempDir()}.Find()
	if !errors.Is(err, ErrServiceNotRunning) {
		t.Errorf("err = %v", err)
	}
}

func TestFindMalformedRegistrationDoesNotLeakContent(t *testing.T) {
	state := t.TempDir()
	writeRegistration(t, state, `{"password":"hunter2"`)
	_, err := Locator{StateHome: state}.Find()
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("err = %v", err)
	}
}

// fakeBinary writes an executable script standing in for opencode, so tests
// never start the user's real service.
func fakeBinary(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartRunsServiceStartThenReadsRegistration(t *testing.T) {
	state := t.TempDir()
	script := `[ "$1 $2" = "service start" ] || exit 9
mkdir -p "$XDG_STATE_HOME/opencode"
echo '` + registration + `' > "$XDG_STATE_HOME/opencode/service.json"`
	ep, err := Locator{Binary: fakeBinary(t, script), StateHome: state}.Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if ep.Password != "pw" {
		t.Errorf("ep = %+v", ep)
	}
}

func TestStartFailureReportsOutput(t *testing.T) {
	bin := fakeBinary(t, `echo boom; exit 3`)
	_, err := Locator{Binary: bin, StateHome: t.TempDir()}.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
}

func TestStartExplicitRunsNothing(t *testing.T) {
	bin := fakeBinary(t, `exit 1`)
	ep, err := Locator{URL: "http://a", Binary: bin}.Start(t.Context())
	if err != nil || ep.URL != "http://a" {
		t.Errorf("ep = %+v, err = %v", ep, err)
	}
}
