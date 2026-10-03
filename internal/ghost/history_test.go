package ghost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const testHistorySession = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func historyResponse(v any) proxyResponse { b, _ := json.Marshal(v); return ok(string(b)) }

func TestHistoryTransportPreservesCompleteFragmentsAndOpaquePagination(t *testing.T) {
	ctx := context.Background()
	discovery := HistorySessionsPage{Version: 1, NodeID: "OFFICEPC", Sessions: []HistorySession{}, NextCursor: "empty-page-continues", Coverage: "retained_only"}
	page := HistoryEventsPage{Version: 1, NodeID: "OFFICEPC", SessionID: testHistorySession, Events: []HistoryEvent{
		{ID: "batch/0001/0000", Sequence: "batch/0001/0000", Kind: "tool_result", Text: strings.Repeat("界", 16000), CallID: "call1", Part: 0, More: true},
		{ID: "batch/0001/0001", Sequence: "batch/0001/0001", Kind: "tool_result", Text: strings.Repeat("tail", 10000), CallID: "call1", Part: 1},
	}, ResumeCursor: "resume-opaque", NextCursor: "next-opaque", Coverage: "retained_only", Gaps: []string{"unsupported record"}}
	end := HistoryEventsPage{Version: 1, NodeID: "OFFICEPC", SessionID: testHistorySession, Events: []HistoryEvent{}, ResumeCursor: "resume-opaque", Coverage: "retained_only", Gaps: []string{"retention boundary"}}
	f := &fakeInvoke{responses: []proxyResponse{historyResponse(discovery), historyResponse(page), historyResponse(end)}}
	c := testClient(f)
	s, e := c.HistorySessions(ctx, "OFFICEPC", "", "correlation")
	if e != nil || s.NextCursor == "" || len(s.Sessions) != 0 {
		t.Fatal("empty page lost continuation", e)
	}
	p, e := c.HistoryEvents(ctx, "OFFICEPC", testHistorySession, "opaque-input", "correlation")
	if e != nil || len(p.Events) != 2 || p.Events[0].Text != page.Events[0].Text || p.Events[1].Text != page.Events[1].Text || p.Events[0].Timestamp != "" || p.Events[1].Sequence != page.Events[1].Sequence || len(p.Gaps) != 1 {
		t.Fatal("fragment fidelity lost", e)
	}
	endPage, e := c.HistoryEvents(ctx, "OFFICEPC", testHistorySession, "resume-opaque", "correlation")
	if e != nil || endPage.ResumeCursor != "resume-opaque" || endPage.NextCursor != "" {
		t.Fatal("end polling contract changed", e)
	}
	for i, ev := range f.events {
		if ev.Task != "internal_api" || ev.Method != "GET" || ev.Body != "" || ev.Query["node_id"] != "OFFICEPC" {
			t.Fatal("unsafe transport", ev)
		}
		if i == 0 && ev.Resource != "/history/sessions" || i > 0 && (ev.Resource != "/history/events" || ev.Query["session_id"] != testHistorySession) {
			t.Fatal(ev)
		}
	}
	if f.events[1].Query["cursor"] != "opaque-input" {
		t.Fatal("cursor modified")
	}
}

func TestHistoryRejectsInvalidScopeAndMismatchedProviderResponses(t *testing.T) {
	f := &fakeInvoke{}
	c := testClient(f)
	for _, tc := range []struct{ node, session, cursor string }{{"../secret", testHistorySession, ""}, {"OFFICEPC", "latest", ""}, {"OFFICEPC", testHistorySession, strings.Repeat("c", 2049)}} {
		if _, e := c.HistoryEvents(context.Background(), tc.node, tc.session, tc.cursor, ""); !errors.Is(e, ErrInvalidRequest) {
			t.Fatal(e)
		}
	}
	if f.calls != 0 {
		t.Fatal("invalid request reached provider")
	}
	for _, raw := range []string{
		`{"version":1,"node_id":"OTHER","sessions":[],"next_cursor":"","coverage":"retained_only"}`,
		`{"version":1,"node_id":"OFFICEPC","sessions":[],"next_cursor":"same","coverage":"retained_only"}`,
		`{"version":1,"node_id":"OFFICEPC","sessions":[],"coverage":"complete"}`,
	} {
		c = testClient(&fakeInvoke{responses: []proxyResponse{ok(raw)}})
		if _, e := c.HistorySessions(context.Background(), "OFFICEPC", "same", ""); !errors.Is(e, ErrUpstream) {
			t.Fatal("invalid response accepted", e)
		}
	}
}

func TestHistoryMalformedFragmentsCannotMasqueradeAsCompletePages(t *testing.T) {
	valid := map[string]any{"id": "batch/0001", "sequence": "batch/0001", "kind": "tool_result", "text": "complete visible content", "part": 0, "more": false}
	invoke := func(events []map[string]any) (HistoryEventsPage, error) {
		page := map[string]any{"version": 1, "node_id": "OFFICEPC", "session_id": testHistorySession, "events": events, "resume_cursor": "resume", "coverage": "retained_only", "gaps": []string{}}
		c := testClient(&fakeInvoke{responses: []proxyResponse{historyResponse(page)}})
		return c.HistoryEvents(context.Background(), "OFFICEPC", testHistorySession, "", "")
	}
	for _, field := range []string{"id", "sequence", "kind", "text", "part", "more"} {
		for _, invalid := range []string{"missing", "null", "wrong type"} {
			t.Run(field+"/"+invalid, func(t *testing.T) {
				fragment := make(map[string]any, len(valid))
				for k, v := range valid {
					fragment[k] = v
				}
				switch invalid {
				case "missing":
					delete(fragment, field)
				case "null":
					fragment[field] = nil
				case "wrong type":
					fragment[field] = map[string]any{"invalid": true}
				}
				page, e := invoke([]map[string]any{fragment})
				if !errors.Is(e, ErrUpstream) || page.Events != nil || page.ResumeCursor != "" {
					t.Fatalf("malformed fragment became usable page: %+v %v", page, e)
				}
			})
		}
	}
	second := map[string]any{"id": "batch/0001", "sequence": "batch/0002", "kind": "tool_result", "text": "distinct content that deduplication would discard", "part": 1, "more": false}
	if page, e := invoke([]map[string]any{valid, second}); !errors.Is(e, ErrUpstream) || page.Events != nil {
		t.Fatal("duplicate event IDs accepted despite distinct sequence/content", e)
	}
	// Explicit zero/false fields and absent optional timestamps remain valid.
	page, e := invoke([]map[string]any{valid})
	if e != nil || len(page.Events) != 1 || page.Events[0].Part != 0 || page.Events[0].More || page.Events[0].Timestamp != "" {
		t.Fatal("valid terminal fragment rejected", e)
	}
}

func TestHistoryErrorStatusesAndClosedInternalTransportFailHonestly(t *testing.T) {
	for status, want := range map[int]error{400: ErrInvalidRequest, 401: ErrNotAuthorized, 403: ErrNotAuthorized, 409: ErrConflict, 410: ErrHistoryGone, 413: ErrHistoryTooLarge, 422: ErrHistoryMalformed, 429: ErrQuota, 503: ErrUnavailable} {
		c := testClient(&fakeInvoke{responses: []proxyResponse{{StatusCode: status, Body: `{"error":"private upstream detail"}`}}})
		_, e := c.HistorySessions(context.Background(), "OFFICEPC", "", "")
		if !errors.Is(e, want) || strings.Contains(e.Error(), "private") {
			t.Fatalf("status%d %v", status, e)
		}
		if status == 401 || status == 403 {
			var he *HTTPError
			if !errors.As(e, &he) || he.StatusCode != status {
				t.Fatal("authorization status lost")
			}
		}
	}
	c := testClient(&fakeInvoke{fnErrors: []string{"Unhandled"}})
	if _, e := c.HistorySessions(context.Background(), "OFFICEPC", "", ""); !errors.Is(e, ErrTransportUnavailable) {
		t.Fatal("closed allowlist looked available", e)
	}
}

func TestScheduledWorkFiltersFleetWideUpstreamAgainstAuthorizedNodes(t *testing.T) {
	events := []ScheduledEvent{
		{EventID: "permitted", Node: "OFFICEPC", Prompt: "redacted prompt", Runs: []ScheduledRun{{RunID: "actual-run", Node: "OFFICEPC"}}},
		{EventID: "hidden-primary", Node: "SECRETPC"},
		{EventID: "hidden-backup", Node: "OFFICEPC", BackupNode: "SECRETPC"},
		{EventID: "hidden-history", Node: "OFFICEPC", Runs: []ScheduledRun{{Node: "SECRETPC"}}},
	}
	f := &fakeInvoke{responses: []proxyResponse{ok(`{"nodes":[{"node_id":"OFFICEPC","state":"offline","connected":false}]}`), historyResponse(map[string]any{"events": events})}}
	got, e := testClient(f).ScheduledWork(context.Background(), "")
	if e != nil || len(got) != 1 || got[0].EventID != "permitted" || got[0].Runs[0].RunID != "actual-run" {
		t.Fatal("unauthorized fleet metadata", got, e)
	}
	if len(f.events) != 2 || f.events[0].Resource != "/nodes" || f.events[1].Resource != "/schedule" {
		t.Fatal("missing fresh node check")
	}
}
