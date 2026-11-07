package opencode

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func infoServer(t *testing.T, password string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, got, _ := r.BasicAuth(); got != password {
			w.WriteHeader(401)
			w.Write([]byte(`{"_tag":"UnauthorizedError","message":"no"}`))
			return
		}
		w.Write([]byte(`{"version":"2.0.32"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStatusDoesNotStartAnything(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	bin := fakeBinary(t, `touch "`+marker+`"`)
	c := NewConnection(Locator{Binary: bin, StateHome: t.TempDir()}, true)
	status := c.Status(t.Context())
	if status.Reachable || status.Source != SourceService || status.Err == nil {
		t.Errorf("status = %+v", status)
	}
	if fileExists(marker) {
		t.Error("Status ran the binary")
	}
}

func TestEnsureStartsServiceOnlyWithAutostart(t *testing.T) {
	srv := infoServer(t, "pw")
	state := t.TempDir()
	reg := `{"url":"` + srv.URL + `","password":"pw"}`
	bin := fakeBinary(t, `mkdir -p "$XDG_STATE_HOME/opencode"; echo '`+reg+`' > "$XDG_STATE_HOME/opencode/service.json"`)
	l := Locator{Binary: bin, StateHome: state}

	if _, err := NewConnection(l, false).Ensure(t.Context()); err == nil {
		t.Fatal("autostart off: want an error")
	}
	if fileExists(filepath.Join(state, "opencode", "service.json")) {
		t.Fatal("autostart off: the service was started")
	}

	info, err := NewConnection(l, true).Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "2.0.32" {
		t.Errorf("info = %+v", info)
	}
}

func TestStartIgnoresAutostartSetting(t *testing.T) {
	srv := infoServer(t, "pw")
	reg := `{"url":"` + srv.URL + `","password":"pw"}`
	bin := fakeBinary(t, `mkdir -p "$XDG_STATE_HOME/opencode"; echo '`+reg+`' > "$XDG_STATE_HOME/opencode/service.json"`)
	c := NewConnection(Locator{Binary: bin, StateHome: t.TempDir()}, false)
	if _, err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitServerIsNeverStarted(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	bin := fakeBinary(t, `touch "`+marker+`"`)
	c := NewConnection(Locator{URL: "http://127.0.0.1:1", Binary: bin}, true)
	if _, err := c.Start(t.Context()); err == nil {
		t.Error("want a connection error")
	}
	if fileExists(marker) {
		t.Error("the binary ran")
	}
}

func TestUnauthorizedRediscoversPasswordOnce(t *testing.T) {
	srv := infoServer(t, "new")
	state := t.TempDir()
	writeRegistration(t, state, `{"url":"`+srv.URL+`","password":"new"}`)
	c := NewConnection(Locator{StateHome: state}, false)
	c.SetEndpoint(Endpoint{URL: srv.URL, Password: "old"})
	if _, err := c.Info(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c.Endpoint().Password != "new" {
		t.Error("the new password was not kept")
	}
}

func TestUnauthorizedRetriesAtMostOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(401)
	}))
	defer srv.Close()
	state := t.TempDir()
	writeRegistration(t, state, `{"url":"`+srv.URL+`","password":"pw-secret"}`)
	c := NewConnection(Locator{StateHome: state}, false)
	c.SetEndpoint(Endpoint{URL: srv.URL})
	_, err := c.Info(t.Context())
	if !IsUnauthorized(err) || calls.Load() != 2 {
		t.Errorf("err = %v, calls = %d", err, calls.Load())
	}
	if strings.Contains(err.Error(), "pw-secret") {
		t.Errorf("error leaks the password: %v", err)
	}
}
