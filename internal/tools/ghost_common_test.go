package tools

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A session listed far down a busy computer's archive (past the old five-page
// lookup bound) is still found within the walk bound.
func TestGhostLookupSessionFindsASessionBeyondFivePages(t *testing.T) {
	pages := make([][]map[string]any, 0, 8)
	for i := 0; i < 7; i++ {
		pages = append(pages, []map[string]any{ghostSession(fmt.Sprintf("00000000-0000-0000-0000-%012d", i), -1)})
	}
	pages = append(pages, []map[string]any{ghostSession(ghostSessionB, 0)})
	f := newGhostFake().on("GET", "/history/sessions", ghostPagedSessions(t, "OFFICEPC", pages...))
	deps, _ := ghostDeps(f)
	s, found, scanned, terr := ghostLookupSession(context.Background(), deps, ghostOwnerInvocation("ghost_session_command", "", nil), "OFFICEPC", ghostSessionB)
	require.Nil(t, terr)
	require.True(t, found, "the session on page eight must be found")
	require.True(t, scanned)
	require.Equal(t, ghostSessionB, s.SessionID)
}

// The walk is bounded: an archive that never ends stops at the bound, and the
// absence of a session is then reported as unproven, never as "not there".
func TestGhostLookupSessionReportsTheWalkBound(t *testing.T) {
	f := newGhostFake().on("GET", "/history/sessions", ghostEndlessSessions("OFFICEPC"))
	deps, _ := ghostDeps(f)
	_, found, scanned, terr := ghostLookupSession(context.Background(), deps, ghostOwnerInvocation("ghost_session_command", "", nil), "OFFICEPC", ghostSessionB)
	require.Nil(t, terr)
	require.False(t, found)
	require.False(t, scanned, "the bound stopped the walk, so absence is not proven")
	calls := f.callsTo("GET", "/history/sessions")
	require.Len(t, calls, ghostSessionWalkPages)
	// Only the upstream's own opaque cursor is ever sent back.
	require.Equal(t, "", calls[0].Query["cursor"])
	for i := 1; i < len(calls); i++ {
		require.Equal(t, fmt.Sprintf("p%d", i), calls[i].Query["cursor"])
	}
}

// A slow upstream must not run the walk past the tool's time limit: after
// ghostSessionWalkBudget of walking it stops, keeps what it read, and says so.
func TestGhostWalkSessionsStopsAtTheTimeBudget(t *testing.T) {
	f := newGhostFake().on("GET", "/history/sessions", ghostEndlessSessions("OFFICEPC"))
	deps, _ := ghostDeps(f)
	// Every clock read advances 5 s, as if each upstream page took 5 s.
	clock := ghostNow
	deps.Now = func() time.Time { clock = clock.Add(5 * time.Second); return clock }
	listing, err := ghostWalkSessions(context.Background(), deps, "corr", "OFFICEPC")
	require.NoError(t, err)
	require.True(t, listing.Partial)
	require.Less(t, listing.Pages, 5, "the walk stops near 12 s, not at the 40-page bound")
	require.Len(t, listing.Sessions, listing.Pages, "what was read is kept")
	require.Contains(t, listing.PartialReason, "to stay within the time limit")
	require.Contains(t, listing.PartialReason, fmt.Sprintf("stopped after %d pages", listing.Pages))
}

// A caller whose deadline is close gets the first page back as a partial
// listing instead of a hard timeout failure.
func TestGhostWalkSessionsStopsNearTheContextDeadline(t *testing.T) {
	f := newGhostFake().on("GET", "/history/sessions", ghostEndlessSessions("OFFICEPC"))
	deps, _ := ghostDeps(f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listing, err := ghostWalkSessions(ctx, deps, "corr", "OFFICEPC")
	require.NoError(t, err)
	require.True(t, listing.Partial)
	require.Equal(t, 1, listing.Pages)
	require.Contains(t, listing.PartialReason, "stopped after 1 pages to stay within the time limit")

	// The summary reports that reason instead of failing.
	f2 := newGhostFake().
		static("GET", "/nodes", 200, ghostNodesBody(ghostNode("OFFICEPC", "live", true, "1.1.60"))).
		on("GET", "/history/sessions", ghostEndlessSessions("OFFICEPC"))
	deps2, _ := ghostDeps(f2)
	out, terr := handleGhostSessionSummary(ctx, deps2, ghostOwnerInvocation("ghost_session_summary", "", nil),
		map[string]any{"node": "OFFICEPC", "windowMinutes": 60})
	require.Nil(t, terr)
	require.Equal(t, true, out["partial"])
	require.Contains(t, out["partialReasons"].([]string)[0], "to stay within the time limit")
}
