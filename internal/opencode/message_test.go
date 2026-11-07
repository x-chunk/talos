package opencode

import "testing"

func TestRecentMessagesDecodesTimelineTypes(t *testing.T) {
	c, _ := record(t, `{"data":[
		{"id":"m3","type":"idle","outcome":"failed","error":{"type":"provider.no-route","message":"no"},"time":{"created":3}},
		{"id":"m2","type":"assistant","content":[{"type":"reasoning","text":"hm"},{"type":"text","text":"done"}]},
		{"id":"m1","type":"user","text":"go","time":{"created":1}},
		{"id":"m0","type":"something-new","error":"plain string"}
	],"cursor":{"previous":null,"next":null}}`)
	messages, err := c.RecentMessages(t.Context(), "ses_1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("len = %d", len(messages))
	}
	idle := messages[0]
	if idle.Outcome != OutcomeFailed || idle.Error == nil || idle.Error.Type != "provider.no-route" || idle.Time.Created != 3 {
		t.Errorf("idle = %+v", idle)
	}
	if got := messages[1].Content[1].Text; got != "done" {
		t.Errorf("text = %q", got)
	}
	if messages[2].Text != "go" {
		t.Errorf("user = %+v", messages[2])
	}
	if messages[3].Error == nil || messages[3].Error.Message != `"plain string"` {
		t.Errorf("unexpected error shape = %+v", messages[3].Error)
	}
}

func TestForms(t *testing.T) {
	c, rec := record(t, `{"data":[{"id":"frm_1","sessionID":"ses_1"}]}`)
	forms, err := c.Forms(t.Context(), "ses_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(forms) != 1 || forms[0].ID != "frm_1" {
		t.Errorf("forms = %+v", forms)
	}
	if err := c.CancelForm(t.Context(), "ses_1", "frm_1"); err != nil {
		t.Fatal(err)
	}
	if rec.method != "DELETE" || rec.path != "/api/session/ses_1/form/frm_1" {
		t.Errorf("request = %s %s", rec.method, rec.path)
	}
}
