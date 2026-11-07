package opencode

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// recorded captures what a handler received so tests can assert on the wire
// format, which is the part OpenCode is strict about.
type recorded struct {
	method, path string
	body         map[string]any
}

func record(t *testing.T, response string) (*Client, *recorded) {
	t.Helper()
	rec := &recorded{}
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rec.method, rec.path = r.Method, r.URL.EscapedPath()
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rec.body); err != nil {
				t.Errorf("request body: %v", err)
			}
		}
		w.Write([]byte(response))
	})
	return c, rec
}

func TestCreateSession(t *testing.T) {
	c, rec := record(t, `{"data":{"id":"ses_1","cost":0,"time":{"created":5}}}`)
	session, err := c.CreateSession(t.Context(), CreateSessionRequest{
		Title:       "t",
		Model:       &ModelRef{ID: "m", ProviderID: "p"},
		Location:    &Location{Directory: "/w"},
		Permissions: []PermissionRule{{Action: "*", Resource: "*", Effect: EffectAllow}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "ses_1" || session.Time.Created != 5 {
		t.Errorf("session = %+v", session)
	}
	if rec.method != "POST" || rec.path != "/api/session" {
		t.Errorf("request = %s %s", rec.method, rec.path)
	}
	if _, ok := rec.body["agent"]; ok {
		t.Error("an unset agent must be omitted")
	}
	if rec.body["model"].(map[string]any)["providerID"] != "p" {
		t.Errorf("body = %v", rec.body)
	}
}

func TestActiveSessions(t *testing.T) {
	c, _ := record(t, `{"data":{"ses_1":{"type":"running"}}}`)
	active, err := c.ActiveSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !active["ses_1"] || len(active) != 1 {
		t.Errorf("active = %v", active)
	}
}

func TestPrompt(t *testing.T) {
	c, rec := record(t, `{"data":{"id":"msg_1"}}`)
	id, err := c.Prompt(t.Context(), "ses_1", "hi", DeliverySteer)
	if err != nil {
		t.Fatal(err)
	}
	if id != "msg_1" || rec.path != "/api/session/ses_1/prompt" {
		t.Errorf("id = %q, path = %q", id, rec.path)
	}
	if rec.body["text"] != "hi" || rec.body["delivery"] != "steer" {
		t.Errorf("body = %v", rec.body)
	}
}

func TestInterruptDecodesBareResponse(t *testing.T) {
	c, rec := record(t, `{"interrupted":true}`)
	interrupted, err := c.Interrupt(t.Context(), "ses_1")
	if err != nil {
		t.Fatal(err)
	}
	if !interrupted || rec.path != "/api/session/ses_1/interrupt" {
		t.Errorf("interrupted = %v, path = %q", interrupted, rec.path)
	}
}

func TestReplyPermission(t *testing.T) {
	c, rec := record(t, ``)
	err := c.ReplyPermission(t.Context(), "ses_child", "per_1", DecisionReject, "no")
	if err != nil {
		t.Fatal(err)
	}
	if rec.path != "/api/session/ses_child/permission/per_1/reply" {
		t.Errorf("path = %q", rec.path)
	}
	if rec.body["decision"] != "reject" || rec.body["message"] != "no" {
		t.Errorf("body = %v", rec.body)
	}
}

func TestPathsEscapeIDs(t *testing.T) {
	c, rec := record(t, `{"data":{}}`)
	if _, err := c.GetSession(t.Context(), "../evil"); err != nil {
		t.Fatal(err)
	}
	if rec.path != "/api/session/..%2Fevil" {
		t.Errorf("path = %q", rec.path)
	}
}

func TestChildSessions(t *testing.T) {
	var query string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Write([]byte(`{"data":[{"id":"ses_c","parentID":"ses_p"}]}`))
	})
	children, err := c.ChildSessions(t.Context(), "ses_p")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].ID != "ses_c" || query != "parentID=ses_p" {
		t.Errorf("children = %+v, query = %q", children, query)
	}
}
