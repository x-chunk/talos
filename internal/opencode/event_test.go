package opencode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseSSEOnCapturedTurn(t *testing.T) {
	f, err := os.Open("testdata/turn.sse")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	counts := map[string]int{}
	var toolName string
	lines := 0
	parseSSE(f, func() { lines++ }, func(e Event) {
		counts[e.Type]++
		if e.Type == EventToolInputStarted {
			data, err := e.Decode()
			if err != nil {
				t.Errorf("decode: %v", err)
			}
			toolName = data.Name
		}
	})
	if counts[EventSessionCreated] != 1 || counts[EventStepStarted] != 2 || counts[EventExecutionSucceeded] != 1 {
		t.Errorf("counts = %v", counts)
	}
	if toolName != "write" {
		t.Errorf("tool = %q", toolName)
	}
	if lines == 0 {
		t.Error("alive was never called")
	}
}

func TestParseSSEEdgeCases(t *testing.T) {
	input := ": heartbeat\n\n" +
		"data: not json\n\n" +
		"data: {\"type\":\n" + "data: \"split\"}\n\n" +
		"event: ignored\r\ndata: {\"type\":\"crlf\"}\r\n\r\n"
	var got []string
	err := parseSSE(strings.NewReader(input), func() {}, func(e Event) { got = append(got, e.Type) })
	if err == nil {
		t.Error("want EOF")
	}
	if fmt.Sprint(got) != "[split crlf]" {
		t.Errorf("got %v", got)
	}
}

func TestEventDecodePermissionAsked(t *testing.T) {
	e := Event{Data: []byte(`{"id":"per_1","sessionID":"ses_c","action":"shell","resources":["ls"]}`)}
	data, err := e.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if data.ID != "per_1" || data.SessionID != "ses_c" || data.Action != "shell" || data.Resources[0] != "ls" {
		t.Errorf("data = %+v", data)
	}
}

var fastStream = StreamOptions{MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}

func TestStreamReconnectsAndReportsEachConnection(t *testing.T) {
	var conns atomic.Int32
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n := conns.Add(1)
		fmt.Fprintf(w, "data: {\"id\":\"e%d\",\"type\":\"x\"}\n\n", n)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var mu sync.Mutex
	var ids []string
	var connects, failures atomic.Int32
	opts := fastStream
	opts.OnConnect = func() { connects.Add(1) }
	opts.OnError = func(error) { failures.Add(1) }
	err := c.Stream(ctx, opts, func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, e.ID)
		if len(ids) == 3 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if fmt.Sprint(ids) != "[e1 e2 e3]" {
		t.Errorf("ids = %v", ids)
	}
	if connects.Load() != 3 || failures.Load() != 2 {
		t.Errorf("connects = %d, failures = %d", connects.Load(), failures.Load())
	}
}

func TestStreamTreatsSilenceAsDeadConnection(t *testing.T) {
	var conns atomic.Int32
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		conns.Add(1)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	opts := fastStream
	opts.IdleTimeout = 20 * time.Millisecond
	opts.OnError = func(err error) {
		if !errors.Is(err, errStreamIdle) {
			t.Errorf("err = %v", err)
		}
		if conns.Load() >= 2 {
			cancel()
		}
	}
	c.Stream(ctx, opts, func(Event) {})
	if conns.Load() < 2 {
		t.Errorf("conns = %d", conns.Load())
	}
}

func TestStreamReportsUnauthorized(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"_tag":"UnauthorizedError","message":"no"}`))
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := fastStream
	var saw401 bool
	opts.OnError = func(err error) {
		saw401 = IsUnauthorized(err)
		cancel()
	}
	c.Stream(ctx, opts, func(Event) { t.Error("no events expected") })
	if !saw401 {
		t.Error("401 not reported")
	}
}
