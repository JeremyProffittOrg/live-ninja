package webapp

// Route-level tests for the M11 history + topics API (history_routes.go)
// over a FakeDynamo-backed store: newest-first listing, every filter facet
// (topic via TREF#, device via FilterExpression, date via sk bounds) and
// their combinations, cursor pagination, the conversation detail view with
// LOG# transcript turns, and the topics CRUD incl. the stable-id merge.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func newHistoryAPIApp(t *testing.T) (*fiber.App, *store.Store) {
	t.Helper()
	st := store.NewWithClient(testutil.NewFakeDynamo(), "live-ninja")
	deps := &Deps{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		return c.Next()
	})
	RegisterHistoryRoutes(app, deps)
	return app, st
}

// seedConversation writes the canonical CONV record plus one TREF row per
// topic — exactly what cmd/topics-extract produces on session end.
func seedConversation(t *testing.T, st *store.Store, ts, sessionID, deviceID string, topicIDs ...string) {
	t.Helper()
	ctx := context.Background()
	err := st.CreateConversation(ctx, "u1", &store.Conversation{
		SessionID: sessionID,
		TS:        ts,
		DeviceID:  deviceID,
		Engine:    "openai-realtime",
		Surface:   "web",
		Title:     "About " + sessionID,
		TopicIDs:  topicIDs,
		TurnCount: 2,
	})
	if err != nil {
		t.Fatalf("seed conversation %s: %v", sessionID, err)
	}
	for _, topicID := range topicIDs {
		if err := st.PutTopicRef(ctx, "u1", &store.TopicRef{
			TopicID: topicID, SessionID: sessionID, TS: ts, DeviceID: deviceID,
		}); err != nil {
			t.Fatalf("seed topic ref %s/%s: %v", topicID, sessionID, err)
		}
	}
}

func seedTopic(t *testing.T, st *store.Store, id, name string) {
	t.Helper()
	if err := st.CreateTopic(context.Background(), "u1", &store.Topic{
		TopicID: id, Name: name, Color: "#2563eb",
	}); err != nil {
		t.Fatalf("seed topic %s: %v", id, err)
	}
}

func convIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	items, _ := body["items"].([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		row, _ := it.(map[string]any)
		sid, _ := row["sessionId"].(string)
		out = append(out, sid)
	}
	return out
}

func TestListConversationsFiltersAndOrder(t *testing.T) {
	app, st := newHistoryAPIApp(t)
	seedTopic(t, st, "t1", "Work")
	seedTopic(t, st, "t2", "Home")
	seedConversation(t, st, "2026-07-01T10:00:00Z", "s1", "devA", "t1")
	seedConversation(t, st, "2026-07-10T12:00:00Z", "s2", "devB", "t1", "t2")
	seedConversation(t, st, "2026-07-15T09:00:00Z", "s3", "devA", "t2")

	// Unfiltered: newest first.
	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/conversations", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d (%v)", resp.StatusCode, body)
	}
	if got := convIDs(t, body); len(got) != 3 || got[0] != "s3" || got[1] != "s2" || got[2] != "s1" {
		t.Errorf("unfiltered order = %v, want [s3 s2 s1]", got)
	}
	// Rows carry the wire projection the clients bind.
	first, _ := body["items"].([]any)[0].(map[string]any)
	for _, field := range []string{"id", "conversationId", "sessionId", "ts", "topicIds", "deviceId", "engine", "title", "turnCount"} {
		if _, ok := first[field]; !ok {
			t.Errorf("conversation row missing field %q", field)
		}
	}

	// Topic facet (TREF# range).
	_, body = doJSON(t, app, http.MethodGet, "/api/v1/conversations?topic=t1", nil)
	if got := convIDs(t, body); len(got) != 2 || got[0] != "s2" || got[1] != "s1" {
		t.Errorf("topic=t1 = %v, want [s2 s1]", got)
	}

	// Device facet (FilterExpression).
	_, body = doJSON(t, app, http.MethodGet, "/api/v1/conversations?device=devA", nil)
	if got := convIDs(t, body); len(got) != 2 || got[0] != "s3" || got[1] != "s1" {
		t.Errorf("device=devA = %v, want [s3 s1]", got)
	}

	// Topic + device combined.
	_, body = doJSON(t, app, http.MethodGet, "/api/v1/conversations?topic=t2&device=devA", nil)
	if got := convIDs(t, body); len(got) != 1 || got[0] != "s3" {
		t.Errorf("topic=t2&device=devA = %v, want [s3]", got)
	}

	// Date range (inclusive date-only bounds).
	_, body = doJSON(t, app, http.MethodGet, "/api/v1/conversations?from=2026-07-05&to=2026-07-12", nil)
	if got := convIDs(t, body); len(got) != 1 || got[0] != "s2" {
		t.Errorf("date range = %v, want [s2]", got)
	}
	// A date-only ?to= includes conversations later that same day.
	_, body = doJSON(t, app, http.MethodGet, "/api/v1/conversations?to=2026-07-15", nil)
	if got := convIDs(t, body); len(got) != 3 {
		t.Errorf("to=2026-07-15 = %v, want all 3 (inclusive day)", got)
	}

	// Topic + date range on the TREF path.
	_, body = doJSON(t, app, http.MethodGet, "/api/v1/conversations?topic=t1&from=2026-07-05", nil)
	if got := convIDs(t, body); len(got) != 1 || got[0] != "s2" {
		t.Errorf("topic=t1&from = %v, want [s2]", got)
	}

	// Validation.
	for _, q := range []string{"?from=notadate", "?to=13/01/2026", "?topic=bad%23id"} {
		resp, _ = doJSON(t, app, http.MethodGet, "/api/v1/conversations"+q, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestListConversationsPagination(t *testing.T) {
	app, st := newHistoryAPIApp(t)
	seedConversation(t, st, "2026-07-01T10:00:00Z", "s1", "devA")
	seedConversation(t, st, "2026-07-02T10:00:00Z", "s2", "devA")
	seedConversation(t, st, "2026-07-03T10:00:00Z", "s3", "devA")

	_, page1 := doJSON(t, app, http.MethodGet, "/api/v1/conversations?limit=2", nil)
	if got := convIDs(t, page1); len(got) != 2 || got[0] != "s3" || got[1] != "s2" {
		t.Fatalf("page1 = %v, want [s3 s2]", got)
	}
	cursor, _ := page1["nextCursor"].(string)
	if cursor == "" {
		t.Fatalf("page1 must return nextCursor")
	}

	_, page2 := doJSON(t, app, http.MethodGet, "/api/v1/conversations?limit=2&cursor="+url.QueryEscape(cursor), nil)
	if got := convIDs(t, page2); len(got) != 1 || got[0] != "s1" {
		t.Fatalf("page2 = %v, want [s1]", got)
	}

	// A cursor from a different filter namespace is rejected, not silently
	// misapplied.
	resp, _ := doJSON(t, app, http.MethodGet, "/api/v1/conversations?topic=t9&cursor="+url.QueryEscape(cursor), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("cross-namespace cursor status = %d, want 400", resp.StatusCode)
	}
}

func TestGetConversationDetail(t *testing.T) {
	app, st := newHistoryAPIApp(t)
	ctx := context.Background()
	seedConversation(t, st, "2026-07-10T12:00:00Z", "sess-1", "devA", "t1")

	// Transcript rows as the sink writes them, including the broker's
	// seq-0 system marker (which the detail view must skip).
	for _, row := range []struct {
		sk, role, text string
	}{
		{"LOG#sess-1#000000", "system", "session-start"},
		{"LOG#sess-1#000001", "user", "hello there"},
		{"LOG#sess-1#000002", "assistant", "hi! how can I help?"},
	} {
		if err := st.ConditionalPut(ctx, "USER#u1", row.sk, map[string]any{
			"role": row.role, "text": row.text, "ts": "2026-07-10T12:00:01Z", "engine": "openai-realtime",
		}, 0); err != nil {
			t.Fatalf("seed turn %s: %v", row.sk, err)
		}
	}

	// Canonical id form: "<ts>#<sessionId>", URL-encoded by clients.
	convID := url.PathEscape("2026-07-10T12:00:00Z#sess-1")
	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/conversations/"+convID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d (%v)", resp.StatusCode, body)
	}
	if body["sessionId"] != "sess-1" || body["title"] != "About sess-1" {
		t.Errorf("detail conversation = %v", body)
	}
	turns, _ := body["turns"].([]any)
	if len(turns) != 2 {
		t.Fatalf("turns len = %d, want 2 (system row skipped)", len(turns))
	}
	turn0, _ := turns[0].(map[string]any)
	if turn0["role"] != "user" || turn0["text"] != "hello there" {
		t.Errorf("turn[0] = %v", turn0)
	}

	// Bare-sessionId fallback resolve.
	resp, body = doJSON(t, app, http.MethodGet, "/api/v1/conversations/sess-1", nil)
	if resp.StatusCode != http.StatusOK || body["sessionId"] != "sess-1" {
		t.Errorf("bare sessionId detail = %d %v", resp.StatusCode, body)
	}

	// Unknown id.
	resp, _ = doJSON(t, app, http.MethodGet, "/api/v1/conversations/nope", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown conversation status = %d, want 404", resp.StatusCode)
	}
}

func TestTopicsCRUD(t *testing.T) {
	app, _ := newHistoryAPIApp(t)

	// Create with server-assigned palette color.
	resp, created := doJSON(t, app, http.MethodPost, "/api/v1/topics", map[string]any{"name": "Work"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create topic status = %d (%v)", resp.StatusCode, created)
	}
	workID, _ := created["id"].(string)
	color, _ := created["color"].(string)
	if workID == "" || !hexColorPattern.MatchString(color) {
		t.Fatalf("created topic = %v", created)
	}

	// Case-insensitive duplicate → 409 with the existing id.
	resp, dup := doJSON(t, app, http.MethodPost, "/api/v1/topics", map[string]any{"name": "work"})
	if resp.StatusCode != http.StatusConflict || dup["topicId"] != workID {
		t.Errorf("duplicate topic = %d %v", resp.StatusCode, dup)
	}

	// Validation.
	resp, _ = doJSON(t, app, http.MethodPost, "/api/v1/topics", map[string]any{"name": ""})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty name status = %d, want 400", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodPost, "/api/v1/topics", map[string]any{"name": "X", "color": "red"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad color status = %d, want 400", resp.StatusCode)
	}

	// Rename + recolor + archive via PATCH.
	resp, patched := doJSON(t, app, http.MethodPatch, "/api/v1/topics/"+workID, map[string]any{
		"name": "Job", "color": "#16a34a", "archived": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d (%v)", resp.StatusCode, patched)
	}
	if patched["name"] != "Job" || patched["color"] != "#16a34a" || patched["archived"] != true {
		t.Errorf("patched topic = %v", patched)
	}

	// List carries items + the populated color palette (picker source).
	_, list := doJSON(t, app, http.MethodGet, "/api/v1/topics", nil)
	if items, _ := list["items"].([]any); len(items) != 1 {
		t.Errorf("topic list len = %d, want 1", len(items))
	}
	if palette, _ := list["palette"].([]any); len(palette) != len(topicColorPalette) {
		t.Errorf("palette missing from topic list")
	}
	_, list = doJSON(t, app, http.MethodGet, "/api/v1/topics?includeArchived=false", nil)
	if items, _ := list["items"].([]any); len(items) != 0 {
		t.Errorf("includeArchived=false should hide the archived topic")
	}

	// PATCH on a missing topic → 404; no-op body → 400.
	resp, _ = doJSON(t, app, http.MethodPatch, "/api/v1/topics/missing-1", map[string]any{"name": "X"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing topic patch status = %d, want 404", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodPatch, "/api/v1/topics/"+workID, map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty patch status = %d, want 400", resp.StatusCode)
	}
}

func TestTopicMergeRepointsHistory(t *testing.T) {
	app, st := newHistoryAPIApp(t)
	seedTopic(t, st, "src", "Coffee chats")
	seedTopic(t, st, "dst", "Social")
	seedConversation(t, st, "2026-07-10T12:00:00Z", "s1", "devA", "src")

	// Merge src → dst.
	resp, merged := doJSON(t, app, http.MethodPatch, "/api/v1/topics/src", map[string]any{"mergedInto": "dst"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("merge status = %d (%v)", resp.StatusCode, merged)
	}
	if merged["mergedInto"] != "dst" || merged["archived"] != true {
		t.Errorf("merged source topic = %v", merged)
	}

	// The conversation now lists under dst, no longer under src, and its
	// topicIds were repointed — the conversation id itself never changed.
	_, underDst := doJSON(t, app, http.MethodGet, "/api/v1/conversations?topic=dst", nil)
	if got := convIDs(t, underDst); len(got) != 1 || got[0] != "s1" {
		t.Errorf("topic=dst after merge = %v, want [s1]", got)
	}
	_, underSrc := doJSON(t, app, http.MethodGet, "/api/v1/conversations?topic=src", nil)
	if got := convIDs(t, underSrc); len(got) != 0 {
		t.Errorf("topic=src after merge = %v, want empty", got)
	}
	items, _ := underDst["items"].([]any)
	row, _ := items[0].(map[string]any)
	if ids, _ := row["topicIds"].([]any); len(ids) != 1 || ids[0] != "dst" {
		t.Errorf("conversation topicIds after merge = %v, want [dst]", row["topicIds"])
	}

	// Self-merge and merge-into-missing are rejected.
	resp, _ = doJSON(t, app, http.MethodPatch, "/api/v1/topics/dst", map[string]any{"mergedInto": "dst"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("self-merge status = %d, want 400", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodPatch, "/api/v1/topics/dst", map[string]any{"mergedInto": "ghost"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("merge into missing status = %d, want 404", resp.StatusCode)
	}
}
