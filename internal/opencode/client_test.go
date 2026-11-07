package opencode

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClient(Endpoint{URL: srv.URL, Password: "secret"}, srv.Client())
}

func TestInfoSendsBasicAuthAndDecodesBareResponse(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != "secret" {
			t.Errorf("basic auth = %q, %q, %v", user, pass, ok)
		}
		if r.URL.Path != "/api/info" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"version":"2.0.32","pid":7,"urls":["http://x"]}`))
	})
	info, err := c.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "2.0.32" || info.PID != 7 {
		t.Errorf("info = %+v", info)
	}
}

func TestDoUnwrapsDataEnvelope(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"version":"wrapped"}}`))
	})
	info, err := c.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "wrapped" {
		t.Errorf("version = %q", info.Version)
	}
}

func TestErrorDecoding(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		wantTag      string
		unauthorized bool
		notFound     bool
	}{
		{"tagged 401", 401, `{"_tag":"UnauthorizedError","message":"no"}`, "UnauthorizedError", true, false},
		{"tagged 404", 404, `{"_tag":"SessionNotFoundError","message":"gone"}`, "SessionNotFoundError", false, true},
		{"not JSON", 502, `<html>bad gateway</html>`, "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			})
			_, err := c.Info(t.Context())
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v", err)
			}
			if apiErr.Status != tt.status || apiErr.Tag != tt.wantTag {
				t.Errorf("apiErr = %+v", apiErr)
			}
			if IsUnauthorized(err) != tt.unauthorized || IsNotFound(err) != tt.notFound {
				t.Errorf("IsUnauthorized = %v, IsNotFound = %v", IsUnauthorized(err), IsNotFound(err))
			}
		})
	}
}

func TestErrorsDoNotLeakPassword(t *testing.T) {
	c := NewClient(Endpoint{URL: "http://127.0.0.1:1", Password: "secret"}, nil)
	_, err := c.Info(t.Context())
	if err == nil {
		t.Fatal("want a connection error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestSetEndpointAppliesToLaterRequests(t *testing.T) {
	var got string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, got, _ = r.BasicAuth()
		w.Write([]byte(`{}`))
	})
	ep := c.Endpoint()
	ep.Password = "rotated"
	c.SetEndpoint(ep)
	if _, err := c.Info(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got != "rotated" {
		t.Errorf("password = %q", got)
	}
}
