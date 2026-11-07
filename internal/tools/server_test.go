package tools

import (
	"context"
	"errors"
	"testing"

	"go.xchunk.org/argon"
	"go.xchunk.org/argon/argontest"
	"go.xchunk.org/talos/internal/opencode"
)

type fakeControl struct {
	running  bool
	startErr error
}

func (c *fakeControl) Status(context.Context) opencode.Status {
	if !c.running {
		return opencode.Status{Source: opencode.SourceService, Err: opencode.ErrServiceNotRunning}
	}
	return opencode.Status{Reachable: true, URL: "http://x", Version: "2.0.32", Source: opencode.SourceService}
}

func (c *fakeControl) Start(context.Context) (opencode.Info, error) {
	if c.startErr != nil {
		return opencode.Info{}, c.startErr
	}
	c.running = true
	return opencode.Info{}, nil
}

func TestServerTools(t *testing.T) {
	control := &fakeControl{}
	s := argon.New("test")
	RegisterServer(s, control)

	status, err := argontest.Call[serverOutput](t, s, "server_status", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if status.Reachable || status.Error == "" || control.running {
		t.Errorf("status = %+v", status)
	}
	status, err = argontest.Call[serverOutput](t, s, "start_server", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Reachable || status.Version != "2.0.32" {
		t.Errorf("status = %+v", status)
	}
	control.startErr = errors.New("no binary")
	if _, err := argontest.Call[serverOutput](t, s, "start_server", struct{}{}); err == nil {
		t.Error("want the failure reported")
	}
}
