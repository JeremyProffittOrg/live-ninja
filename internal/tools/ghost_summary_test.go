package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func ghostConversation() []map[string]any {
	return []map[string]any{
		ghostHistoryEvent(1, "user", "Fix the flaky retry test in the store package and run the suite."),
		ghostHistoryEvent(2, "assistant", "Looking at internal/store/rules_test.go now."),
		ghostHistoryEvent(3, "tool_call", `{"command":"go test ./internal/store"}`),
		ghostHistoryEvent(4, "tool_result", "ok  	internal/store	1.2s"),
		ghostHistoryEvent(5, "user", "Great, make the timeout configurable too."),
		ghostHistoryEvent(6, "assistant", "Done: the timeout is read from RULES_TIMEOUT. Should I push to main?"),
	}
}

func TestGhostSessionSummarySingleSession(t *testing.T) {
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		on("GET", "/history/events", func(ev ghostEvent) (int, string) {
			require.Equal(t, "OFFICEPC", ev.Query["node_id"])
			require.Equal(t, ghostSessionA, ev.Query["session_id"])
			return 200, ghostEventsBody("OFFICEPC", ghostSessionA, "", ghostConversation(), "one unsupported record")
		})
	deps, _ := ghostDeps(f)
	res := newTestRegistry(t, deps).Invoke(context.Background(),
		ghostOwnerInvocation("ghost_session_summary", "", map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA}))
	require.True(t, res.OK, "%+v", res.Error)
	out := res.Output
	require.Equal(t, "session", out["scope"])
	require.Equal(t, "extractive", out["method"])
	require.Equal(t, false, out["partial"])
	require.Empty(t, out["partialReasons"])
	sessions := out["sessions"].([]map[string]any)
	require.Len(t, sessions, 1)
	s := sessions[0]
	require.Equal(t, 6, s["eventsRead"])
	require.Equal(t, 6, s["eventsSummarized"])
	require.Equal(t, map[string]int{"user": 2, "assistant": 2, "tool_call": 1, "tool_result": 1}, s["countsByKind"])
	require.Equal(t, 1, s["toolCalls"])
	require.Equal(t, "Bash", s["lastTool"])
	require.Equal(t, "Great, make the timeout configurable too.", s["lastUserPrompt"])
	require.Contains(t, s["lastAssistantMessage"], "Should I push to main?")
	require.Equal(t, true, s["awaitingInput"])
	require.Contains(t, s["pendingQuestion"], "Should I push to main?")
	require.Equal(t, "assistant", s["lastEventKind"])
	require.Equal(t, []string{"one unsupported record"}, s["gaps"])
	require.NotEmpty(t, s["firstTimestamp"])
	require.NotEmpty(t, s["lastTimestamp"])
	// No listing was consulted, so activity falls back to the last event time.
	require.Equal(t, ghostActivityActive, s["activity"])
	require.Equal(t, "last event timestamp", s["activitySource"])
	require.Empty(t, f.callsTo("GET", "/history/sessions"))
}

// ghost-cli splits a message over 48 KiB into fragments (part n, more=true
// until the last). The summary joins consecutive fragments of one block back
// into one event before counting or quoting, so a three-fragment reply is one
// assistant event whose quoted text is the whole message, and tool fragments
// join only within the same call_id.
func TestGhostSessionSummaryJoinsFragmentsOfOneMessage(t *testing.T) {
	frag := func(i int, kind, text string, part int, more bool, callID string) map[string]any {
		ev := ghostHistoryEvent(i, kind, text)
		ev["part"] = part
		ev["more"] = more
		if callID != "" {
			ev["call_id"] = callID
			ev["name"] = "Bash"
		} else {
			delete(ev, "call_id")
			delete(ev, "name")
		}
		return ev
	}
	events := []map[string]any{
		frag(1, "user", "Explain the retry logic in detail.", 0, false, ""),
		frag(2, "tool_call", `{"command":"go doc ./internal/store"}`, 0, false, "call-A"),
		frag(3, "tool_result", "package store // first half of the doc output… ", 0, true, "call-A"),
		frag(4, "tool_result", "and the second half of it.", 1, false, "call-A"),
		frag(5, "tool_result", "a different call's output", 0, false, "call-B"),
		frag(6, "assistant", "The retry logic has three parts. ", 0, true, ""),
		frag(7, "assistant", "First the backoff, then the jitter, ", 1, true, ""),
		frag(8, "assistant", "then the ceiling. Shall I make the ceiling configurable?", 2, false, ""),
	}
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		static("GET", "/history/events", 200, ghostEventsBody("OFFICEPC", ghostSessionA, "", events))
	deps, _ := ghostDeps(f)
	out, terr := handleGhostSessionSummary(context.Background(), deps, ghostOwnerInvocation("ghost_session_summary", "", nil),
		map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA})
	require.Nil(t, terr)
	require.Equal(t, false, out["partial"])
	s := out["sessions"].([]map[string]any)[0]
	require.Equal(t, 8, s["fragmentsRead"])
	require.Equal(t, 5, s["eventsRead"], "8 fragments are 5 messages")
	require.Equal(t, 5, s["eventsSummarized"])
	require.Equal(t, map[string]int{"user": 1, "tool_call": 1, "tool_result": 2, "assistant": 1}, s["countsByKind"])
	require.Equal(t, 1, s["toolCalls"])
	require.Equal(t, "The retry logic has three parts. First the backoff, then the jitter, then the ceiling. Shall I make the ceiling configurable?",
		s["lastAssistantMessage"], "the whole message, not its trailing fragment")
	require.Equal(t, true, s["awaitingInput"])
	require.Equal(t, "assistant", s["lastEventKind"])

	// A fragment with a gap in its part numbers is not joined across the gap.
	broken := []map[string]any{
		frag(1, "assistant", "part zero ", 0, true, ""),
		frag(2, "assistant", "part two", 2, false, ""),
	}
	f2 := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		static("GET", "/history/events", 200, ghostEventsBody("OFFICEPC", ghostSessionA, "", broken))
	deps2, _ := ghostDeps(f2)
	out, terr = handleGhostSessionSummary(context.Background(), deps2, ghostOwnerInvocation("ghost_session_summary", "", nil),
		map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA})
	require.Nil(t, terr)
	s = out["sessions"].([]map[string]any)[0]
	require.Equal(t, 2, s["eventsRead"])
	require.NotContains(t, s, "fragmentsRead")
	require.Equal(t, "part two", s["lastAssistantMessage"])
}

func TestGhostSessionSummaryAllSessionsInWindow(t *testing.T) {
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		static("GET", "/history/sessions", 200, ghostSessionsBody("OFFICEPC", "",
			ghostSession(ghostSessionB, 20*time.Minute),
			ghostSession(ghostSessionA, 10*time.Minute),
			ghostSession(ghostSessionC, 3*time.Hour),
			ghostSession(ghostSessionD, -1))).
		on("GET", "/history/events", func(ev ghostEvent) (int, string) {
			sid := ev.Query["session_id"]
			switch sid {
			case ghostSessionA:
				return 200, ghostEventsBody("OFFICEPC", sid, "", ghostConversation())
			case ghostSessionB:
				return 200, ghostEventsBody("OFFICEPC", sid, "", []map[string]any{
					ghostHistoryEvent(1, "user", "Rotate the Twilio key"),
					ghostHistoryEvent(2, "assistant", "Rotated and verified."),
				})
			}
			t.Fatalf("summarized a session outside the window: %s", sid)
			return 500, ""
		})
	deps, _ := ghostDeps(f)
	out, terr := handleGhostSessionSummary(context.Background(), deps, ghostOwnerInvocation("ghost_session_summary", "", nil),
		map[string]any{"node": "OFFICEPC", "windowMinutes": 60})
	require.Nil(t, terr)
	require.Equal(t, "window", out["scope"])
	require.Equal(t, 60, out["windowMinutes"])
	require.Equal(t, 4, out["retainedSessions"])
	require.Equal(t, 1, out["sessionsWithoutTimestamp"])
	require.Equal(t, false, out["partial"])
	sessions := out["sessions"].([]map[string]any)
	require.Len(t, sessions, 2, "the 3-hour-old and the undated sessions are outside the window")
	require.Equal(t, ghostSessionA, sessions[0]["sessionId"], "most recent first")
	require.Equal(t, ghostSessionB, sessions[1]["sessionId"])
	require.Equal(t, ghostActivityActive, sessions[0]["activity"])
	require.Equal(t, ghostActivityActive, sessions[1]["activity"])
	require.Equal(t, false, sessions[1]["awaitingInput"])
	require.Equal(t, "Rotated and verified.", sessions[1]["lastAssistantMessage"])
	require.NotContains(t, out, "message")
	require.Len(t, f.callsTo("GET", "/history/events"), 2)
}

func TestGhostSessionSummaryTruncatesAndSaysSo(t *testing.T) {
	events := make([]map[string]any, 0, 256)
	for i := 0; i < 256; i++ {
		kind := "assistant"
		if i%2 == 0 {
			kind = "user"
		}
		events = append(events, ghostHistoryEvent(i, kind, fmt.Sprintf("turn %d", i)))
	}
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		static("GET", "/history/events", 200, ghostEventsBody("OFFICEPC", ghostSessionA, "", events))
	deps, _ := ghostDeps(f)
	out, terr := handleGhostSessionSummary(context.Background(), deps, ghostOwnerInvocation("ghost_session_summary", "", nil),
		map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA})
	require.Nil(t, terr)
	require.Equal(t, true, out["partial"])
	reasons := out["partialReasons"].([]string)
	require.Len(t, reasons, 1)
	require.Contains(t, reasons[0], "last 200 events")
	s := out["sessions"].([]map[string]any)[0]
	require.Equal(t, 256, s["eventsRead"])
	require.Equal(t, 200, s["eventsSummarized"])
	require.Equal(t, "turn 254", s["lastUserPrompt"], "the tail, not the head, is kept")
	require.Equal(t, "turn 255", s["lastAssistantMessage"])
}

func TestGhostSessionSummaryByteBoundIsAlsoHonored(t *testing.T) {
	// 50 events of 2 KB each is 100 KB: over the 48 KB bound, under the 200-event bound.
	big := strings.Repeat("x", 2048)
	events := make([]map[string]any, 0, 50)
	for i := 0; i < 50; i++ {
		events = append(events, ghostHistoryEvent(i, "tool_result", big))
	}
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		static("GET", "/history/events", 200, ghostEventsBody("OFFICEPC", ghostSessionA, "", events))
	deps, _ := ghostDeps(f)
	out, terr := handleGhostSessionSummary(context.Background(), deps, ghostOwnerInvocation("ghost_session_summary", "", nil),
		map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA})
	require.Nil(t, terr)
	require.Equal(t, true, out["partial"])
	s := out["sessions"].([]map[string]any)[0]
	require.Equal(t, 50, s["eventsRead"])
	require.Equal(t, 24, s["eventsSummarized"], "48 KB / 2 KB")
}

func TestGhostSessionSummaryPageBoundIsHonored(t *testing.T) {
	pagesServed := 0
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		on("GET", "/history/events", func(ev ghostEvent) (int, string) {
			pagesServed++
			events := make([]map[string]any, 0, 10)
			for i := 0; i < 10; i++ {
				events = append(events, ghostHistoryEvent(pagesServed*100+i, "assistant", fmt.Sprintf("page %d line %d", pagesServed, i)))
			}
			return 200, ghostEventsBody("OFFICEPC", ghostSessionA, fmt.Sprintf("more-%d", pagesServed), events)
		})
	deps, _ := ghostDeps(f)
	out, terr := handleGhostSessionSummary(context.Background(), deps, ghostOwnerInvocation("ghost_session_summary", "", nil),
		map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA})
	require.Nil(t, terr)
	require.Equal(t, ghostSummaryMaxPages, pagesServed, "reads stop at the page bound")
	require.Equal(t, true, out["partial"])
	require.Contains(t, strings.Join(out["partialReasons"].([]string), "\n"), "longer than 4 pages")
	s := out["sessions"].([]map[string]any)[0]
	require.Equal(t, 40, s["eventsRead"])
}

func TestGhostSessionSummaryEmptyHistory(t *testing.T) {
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		static("GET", "/history/events", 200, ghostEventsBody("OFFICEPC", ghostSessionA, "", nil,
			"Only retained, consented uploads are available."))
	deps, _ := ghostDeps(f)
	out, terr := handleGhostSessionSummary(context.Background(), deps, ghostOwnerInvocation("ghost_session_summary", "", nil),
		map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA})
	require.Nil(t, terr)
	require.Equal(t, false, out["partial"])
	s := out["sessions"].([]map[string]any)[0]
	require.Equal(t, 0, s["eventsRead"])
	require.Equal(t, 0, s["eventsSummarized"])
	require.Contains(t, s["message"], "no retained events")
	require.Equal(t, ghostActivityUnknown, s["activity"])
	require.NotContains(t, s, "lastUserPrompt")
	require.NotContains(t, s, "awaitingInput")
	require.Len(t, s["gaps"], 1)

	// A window with nothing active says so rather than returning silence.
	f2 := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		static("GET", "/history/sessions", 200, ghostSessionsBody("OFFICEPC", ""))
	deps2, _ := ghostDeps(f2)
	out, terr = handleGhostSessionSummary(context.Background(), deps2, ghostOwnerInvocation("ghost_session_summary", "", nil),
		map[string]any{"node": "OFFICEPC"})
	require.Nil(t, terr)
	require.Len(t, out["sessions"], 0)
	require.Contains(t, out["message"], "no retained session")
	require.Equal(t, ghostSummaryDefaultWin, out["windowMinutes"])
}

func TestGhostSessionSummaryErrors(t *testing.T) {
	f := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		static("GET", "/history/events", 410, `{"error":"gone"}`)
	deps, _ := ghostDeps(f)
	inv := ghostOwnerInvocation("ghost_session_summary", "", nil)
	_, terr := handleGhostSessionSummary(context.Background(), deps, inv, map[string]any{"node": "OFFICEPC", "sessionId": ghostSessionA})
	require.NotNil(t, terr)
	require.Equal(t, CodeNotFound, terr.Code, "expired history is not-found, not upstream")

	_, terr = handleGhostSessionSummary(context.Background(), deps, inv, map[string]any{"node": "OFFICEPC", "sessionId": "latest"})
	require.NotNil(t, terr)
	require.Equal(t, CodeInvalidArgs, terr.Code)

	_, terr = handleGhostSessionSummary(context.Background(), deps, inv, map[string]any{"node": "BASEMENT"})
	require.NotNil(t, terr)
	require.Equal(t, CodeNotFound, terr.Code)
}
