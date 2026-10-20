package ghost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The type literals are ghost-cli's CommandType values
// (lambda/command/envelope.go). Pinned as literals so a rename on either side
// fails here instead of as a 400 "unknown command type" at the far end.
func TestCommandTypesMatchGhostCLI(t *testing.T) {
	want := []string{"NUDGE", "ANSWER", "PAUSE", "RESUME"}
	if strings.Join(AllowedCommandTypes, ",") != strings.Join(want, ",") {
		t.Fatalf("AllowedCommandTypes = %v, want %v", AllowedCommandTypes, want)
	}
	for _, forbidden := range []string{"KILL", "LAUNCH", "SAVE_LOCAL", "LIST_SAVED", "KILL_ALL", "UPDATE_AGENT", "CONFIG_PATCH", "nudge", ""} {
		if CommandAllowed(forbidden) {
			t.Errorf("CommandAllowed(%q) = true", forbidden)
		}
	}
	if MaxCommandText != 280 {
		t.Fatalf("MaxCommandText = %d, want ghost-cli maxNudgeText 280", MaxCommandText)
	}
}

func TestCommandSendsGhostCLIEnvelope(t *testing.T) {
	for _, tc := range []struct {
		typ, text string
	}{
		{CommandNudge, "please run the tests first"},
		{CommandAnswer, "Yes"},
		{CommandPause, ""},
		{CommandResume, ""},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			f := &fakeInvoke{responses: []proxyResponse{{StatusCode: 202, Body: `{"command_id":"019a-cmd"}`}}}
			c := testClient(f)
			res, err := c.Command(context.Background(), CommandRequest{
				Type: tc.typ, Node: "OFFICEPC", SessionID: testHistorySession, Text: tc.text,
			}, "corr-1")
			if err != nil {
				t.Fatal(err)
			}
			if res.CommandID != "019a-cmd" {
				t.Fatalf("command id = %q", res.CommandID)
			}
			if len(f.events) != 1 {
				t.Fatalf("made %d calls, want 1", len(f.events))
			}
			ev := f.events[0]
			if ev.Method != "POST" || ev.Resource != "/command" || ev.CorrelationID != "corr-1" || len(ev.Query) != 0 {
				t.Fatalf("transport = %+v", ev)
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(ev.Body), &body); err != nil {
				t.Fatal(err)
			}
			if body["type"] != tc.typ || body["node"] != "OFFICEPC" {
				t.Fatalf("body = %v", body)
			}
			params, _ := body["params"].(map[string]any)
			if params["session_id"] != testHistorySession {
				t.Fatalf("params = %v", params)
			}
			text, hasText := params["text"]
			if tc.text == "" && hasText {
				t.Fatalf("%s carried text %v", tc.typ, text)
			}
			if tc.text != "" && text != tc.text {
				t.Fatalf("text = %v, want %q", text, tc.text)
			}
			// The principal is never ours to name.
			for _, key := range []string{"principal", "role", "scope"} {
				if _, present := body[key]; present {
					t.Fatalf("body names %s", key)
				}
			}
		})
	}
}

func TestCommandRefusesBadRequestsBeforeInvoking(t *testing.T) {
	f := &fakeInvoke{}
	c := testClient(f)
	long := strings.Repeat("x", MaxCommandText+1)
	for name, req := range map[string]CommandRequest{
		"kill":              {Type: "KILL", Node: "OFFICEPC", SessionID: testHistorySession},
		"launch":            {Type: "LAUNCH", Node: "OFFICEPC", SessionID: testHistorySession},
		"lowercase":         {Type: "nudge", Node: "OFFICEPC", SessionID: testHistorySession, Text: "x"},
		"bad node":          {Type: CommandNudge, Node: "../x", SessionID: testHistorySession, Text: "x"},
		"empty node":        {Type: CommandNudge, Node: "", SessionID: testHistorySession, Text: "x"},
		"bad session":       {Type: CommandNudge, Node: "OFFICEPC", SessionID: "latest", Text: "x"},
		"nudge no text":     {Type: CommandNudge, Node: "OFFICEPC", SessionID: testHistorySession, Text: "   "},
		"answer no text":    {Type: CommandAnswer, Node: "OFFICEPC", SessionID: testHistorySession},
		"nudge too long":    {Type: CommandNudge, Node: "OFFICEPC", SessionID: testHistorySession, Text: long},
		"nudge multi-line":  {Type: CommandNudge, Node: "OFFICEPC", SessionID: testHistorySession, Text: "a\nb"},
		"pause with text":   {Type: CommandPause, Node: "OFFICEPC", SessionID: testHistorySession, Text: "x"},
		"resume with text":  {Type: CommandResume, Node: "OFFICEPC", SessionID: testHistorySession, Text: "x"},
		"byte-bounded text": {Type: CommandNudge, Node: "OFFICEPC", SessionID: testHistorySession, Text: strings.Repeat("界", 94)},
	} {
		if _, err := c.Command(context.Background(), req, ""); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if f.calls != 0 {
		t.Fatalf("invalid requests reached the provider %d times", f.calls)
	}
}

func TestCommandMapsUpstreamOutcomes(t *testing.T) {
	req := CommandRequest{Type: CommandPause, Node: "OFFICEPC", SessionID: testHistorySession}
	for _, tc := range []struct {
		name string
		resp proxyResponse
		want error
	}{
		{"forbidden", proxyResponse{StatusCode: 403, Body: `{"error":"forbidden"}`}, ErrNotAuthorized},
		{"bad request", proxyResponse{StatusCode: 400, Body: `{"error":"invalid node"}`}, ErrInvalidRequest},
		{"publisher down", proxyResponse{StatusCode: 503, Body: `{}`}, ErrUnavailable},
		{"publish failed", proxyResponse{StatusCode: 502, Body: `{}`}, ErrUpstream},
		{"no id", proxyResponse{StatusCode: 202, Body: `{"command_id":""}`}, ErrUpstream},
		{"garbage", proxyResponse{StatusCode: 202, Body: `not json`}, ErrUpstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(&fakeInvoke{responses: []proxyResponse{tc.resp}})
			if _, err := c.Command(context.Background(), req, ""); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	// Not configured: refused locally.
	unconfigured := New(Config{})
	if _, err := unconfigured.Command(context.Background(), req, ""); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestHistorySessionsCarryUpdatedAt(t *testing.T) {
	page := map[string]any{"version": 1, "node_id": "OFFICEPC", "coverage": "retained_only", "next_cursor": "",
		"sessions": []map[string]any{
			{"session_id": testHistorySession, "date": "2026-10-18", "updated_at": "2026-10-18T10:00:00.5Z"},
			{"session_id": "agent-0123456789abcdef", "date": "2026-10-17"},
		}}
	c := testClient(&fakeInvoke{responses: []proxyResponse{historyResponse(page)}})
	got, err := c.HistorySessions(context.Background(), "OFFICEPC", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Sessions[0].UpdatedAt != "2026-10-18T10:00:00.5Z" || got.Sessions[1].UpdatedAt != "" {
		t.Fatalf("updated_at lost: %+v", got.Sessions)
	}
	for _, s := range []string{testHistorySession, "agent-0123456789abcdef"} {
		if !ValidSessionID(s) {
			t.Errorf("ValidSessionID(%q) = false", s)
		}
	}
	for _, n := range []string{"OFFICEPC", "node:01_x-y"} {
		if !ValidNodeID(n) {
			t.Errorf("ValidNodeID(%q) = false", n)
		}
	}
	if ValidNodeID("") || ValidNodeID("a b") || ValidSessionID("") || ValidSessionID("latest") {
		t.Error("invalid identifiers accepted")
	}
}
