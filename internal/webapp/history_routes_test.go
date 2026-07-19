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
	"time"

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

	// Transcript rows as the sink writes them (including the broker's
	// seq-0 system marker, which the detail view must skip) plus the tool
	// router's role=tool audit rows. Tool seq values come from the
	// millisecond clock (large numbers), so sk order does NOT interleave
	// them with the sink's 0,1,2… — the route must merge by ts instead.
	for _, row := range []struct {
		sk, role, text, ts, output string
	}{
		{"LOG#sess-1#000000", "system", "session-start", "2026-07-10T12:00:00Z", ""},
		{"LOG#sess-1#000001", "user", "hello there", "2026-07-10T12:00:01Z", ""},
		{"LOG#sess-1#000002", "assistant", "hi! how can I help?", "2026-07-10T12:00:05Z", ""},
		// Ran between the two spoken turns; must land between them.
		{"LOG#sess-1#734512", "tool", `tool=weather outcome=ok callId=call_1 args={"city":"Tampa"}`,
			"2026-07-10T12:00:03.250Z", `{"tempF":90}`},
		// Failed invocation: " error=<code>" suffix parsed off the args.
		{"LOG#sess-1#734890", "tool", `tool=send_email outcome=error callId=call_2 args={"to":"x"} error=forbidden`,
			"2026-07-10T12:00:06Z", ""},
	} {
		attrs := map[string]any{
			"role": row.role, "text": row.text, "ts": row.ts, "engine": "openai-realtime",
		}
		if row.output != "" {
			attrs["output"] = row.output
		}
		if err := st.ConditionalPut(ctx, "USER#u1", row.sk, attrs, 0); err != nil {
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
	if len(turns) != 4 {
		t.Fatalf("turns len = %d, want 4 (system row skipped, tool rows merged)", len(turns))
	}
	wantRoles := []string{"user", "tool", "assistant", "tool"}
	for i, want := range wantRoles {
		row, _ := turns[i].(map[string]any)
		if row["role"] != want {
			t.Fatalf("turns[%d].role = %v, want %s (ts-merged order %v)", i, row["role"], want, turns)
		}
	}
	turn0, _ := turns[0].(map[string]any)
	if turn0["text"] != "hello there" {
		t.Errorf("turn[0] = %v", turn0)
	}
	// Parsed tool-call fields on the ok invocation, output snippet included.
	toolOK, _ := turns[1].(map[string]any)
	if toolOK["tool"] != "weather" || toolOK["outcome"] != "ok" || toolOK["callId"] != "call_1" ||
		toolOK["args"] != `{"city":"Tampa"}` || toolOK["output"] != `{"tempF":90}` {
		t.Errorf("ok tool entry = %v", toolOK)
	}
	if _, hasErr := toolOK["error"]; hasErr {
		t.Errorf("ok tool entry must not carry an error field: %v", toolOK)
	}
	// The failed invocation: error code split off the args tail.
	toolErr, _ := turns[3].(map[string]any)
	if toolErr["tool"] != "send_email" || toolErr["outcome"] != "error" ||
		toolErr["error"] != "forbidden" || toolErr["args"] != `{"to":"x"}` {
		t.Errorf("error tool entry = %v", toolErr)
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

func TestDeleteTopicRoute(t *testing.T) {
	app, st := newHistoryAPIApp(t)
	seedTopic(t, st, "gone", "Doomed")
	seedTopic(t, st, "keep", "Kept")
	seedConversation(t, st, "2026-07-01T10:00:00Z", "s1", "devA", "gone")
	seedConversation(t, st, "2026-07-02T10:00:00Z", "s2", "devA", "keep")

	resp, body := doJSON(t, app, http.MethodDelete, "/api/v1/topics/gone", nil)
	if resp.StatusCode != http.StatusOK || body["ok"] != true {
		t.Fatalf("delete status = %d body = %v", resp.StatusCode, body)
	}

	// The topic no longer appears in the taxonomy.
	_, list := doJSON(t, app, http.MethodGet, "/api/v1/topics", nil)
	items, _ := list["items"].([]any)
	for _, it := range items {
		row, _ := it.(map[string]any)
		if row["id"] == "gone" {
			t.Errorf("deleted topic still listed: %v", row)
		}
	}

	// Filtering by the deleted topic returns empty — its TREF refs are gone.
	_, filtered := doJSON(t, app, http.MethodGet, "/api/v1/conversations?topic=gone", nil)
	if got := convIDs(t, filtered); len(got) != 0 {
		t.Errorf("conversations under deleted topic = %v, want empty", got)
	}

	// The other topic and its conversation are untouched.
	_, keepList := doJSON(t, app, http.MethodGet, "/api/v1/conversations?topic=keep", nil)
	if got := convIDs(t, keepList); len(got) != 1 || got[0] != "s2" {
		t.Errorf("conversations under surviving topic = %v, want [s2]", got)
	}

	// The conversation record itself still exists (untouched, not deleted).
	resp, conv := doJSON(t, app, http.MethodGet,
		"/api/v1/conversations/"+url.PathEscape("2026-07-01T10:00:00Z#s1"), nil)
	if resp.StatusCode != http.StatusOK || conv["sessionId"] != "s1" {
		t.Errorf("conversation after topic delete = %d %v", resp.StatusCode, conv)
	}

	// Re-deleting (or deleting an unknown id) is 404; a malformed id is 400.
	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/topics/gone", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("re-delete status = %d, want 404", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/topics/nope-never-existed", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete missing topic status = %d, want 404", resp.StatusCode)
	}
	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/topics/"+url.PathEscape("bad#id"), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("delete malformed id status = %d, want 400", resp.StatusCode)
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

// ---- Task #7: turnsOver facet, persisted cost, raw turns, cost summary ----

func TestListConversationsTurnsOverAndCost(t *testing.T) {
	app, st := newHistoryAPIApp(t)
	ctx := context.Background()

	mk := func(sid, ts string, turns int, usd float64) {
		conv := &store.Conversation{
			SessionID: sid, TS: ts, DeviceID: "web", Surface: "web",
			TurnCount: turns, CostUSD: usd,
		}
		if usd > 0 {
			conv.CostTextTokens = 100
			conv.CostAudioTokens = 200
		}
		if err := st.CreateConversation(ctx, "u1", conv); err != nil {
			t.Fatalf("seed conversation %s: %v", sid, err)
		}
	}
	mk("short", "2026-07-01T10:00:00Z", 3, 0.05)
	mk("long", "2026-07-02T10:00:00Z", 12, 0.25)
	mk("nocost", "2026-07-03T10:00:00Z", 9, 0)

	// turnsOver keeps strictly-greater-than matches only.
	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/conversations?turnsOver=8", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("turnsOver status = %d (%v)", resp.StatusCode, body)
	}
	if got := convIDs(t, body); len(got) != 2 || got[0] != "nocost" || got[1] != "long" {
		t.Errorf("turnsOver=8 = %v, want [nocost long]", got)
	}

	// Cost fields ship on costed rows and are omitted on uncosted ones.
	items, _ := body["items"].([]any)
	byID := map[string]map[string]any{}
	for _, it := range items {
		row, _ := it.(map[string]any)
		sid, _ := row["sessionId"].(string)
		byID[sid] = row
	}
	if usd, ok := byID["long"]["costUsd"].(float64); !ok || usd != 0.25 {
		t.Errorf("long costUsd = %v, want 0.25", byID["long"]["costUsd"])
	}
	if tok, ok := byID["long"]["costTextTokens"].(float64); !ok || tok != 100 {
		t.Errorf("long costTextTokens = %v, want 100", byID["long"]["costTextTokens"])
	}
	if _, present := byID["nocost"]["costUsd"]; present {
		t.Errorf("nocost row must omit costUsd, got %v", byID["nocost"]["costUsd"])
	}

	// Out-of-range turnsOver is rejected.
	resp, _ = doJSON(t, app, http.MethodGet, "/api/v1/conversations?turnsOver=-1", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("turnsOver=-1 status = %d, want 400", resp.StatusCode)
	}
}

func TestGetConversationRawTurns(t *testing.T) {
	app, st := newHistoryAPIApp(t)
	ctx := context.Background()
	seedConversation(t, st, "2026-07-10T12:00:00Z", "sess-raw", "web")

	rows := []struct{ sk, role, text, ts string }{
		{"LOG#sess-raw#000000", "system", "session-start", "2026-07-10T12:00:00Z"},
		{"LOG#sess-raw#000001", "user", "hello", "2026-07-10T12:00:01Z"},
		{"LOG#sess-raw#000002", "assistant", "hi!", "2026-07-10T12:00:02Z"},
	}
	for _, row := range rows {
		attrs := map[string]any{"role": row.role, "text": row.text, "ts": row.ts, "engine": "openai-realtime"}
		if err := st.ConditionalPut(ctx, "USER#u1", row.sk, attrs, 0); err != nil {
			t.Fatalf("seed turn %s: %v", row.sk, err)
		}
	}

	id := url.PathEscape("2026-07-10T12:00:00Z#sess-raw")

	// Default response has no rawTurns.
	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/conversations/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail status = %d (%v)", resp.StatusCode, body)
	}
	if _, present := body["rawTurns"]; present {
		t.Errorf("rawTurns must be absent without ?raw=1")
	}

	// ?raw=1 ships every LOG# row verbatim, INCLUDING the system marker.
	resp, body = doJSON(t, app, http.MethodGet, "/api/v1/conversations/"+id+"?raw=1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw detail status = %d (%v)", resp.StatusCode, body)
	}
	rawTurns, _ := body["rawTurns"].([]any)
	if len(rawTurns) != 3 {
		t.Fatalf("rawTurns count = %d, want 3 (incl. system)", len(rawTurns))
	}
	first, _ := rawTurns[0].(map[string]any)
	if first["role"] != "system" || first["sk"] != "LOG#sess-raw#000000" || first["seq"] != "000000" {
		t.Errorf("first raw row = %v", first)
	}
	// The merged turns view still skips the system marker.
	turns, _ := body["turns"].([]any)
	if len(turns) != 2 {
		t.Errorf("merged turns = %d, want 2", len(turns))
	}
}

func TestCostSummaryRoute(t *testing.T) {
	app, st := newHistoryAPIApp(t)
	ctx := context.Background()

	now := time.Now().UTC()
	monthTS := time.Date(now.Year(), now.Month(), 1, 6, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if err := st.CreateConversation(ctx, "u1", &store.Conversation{
		SessionID: "cur", TS: monthTS, TurnCount: 4, CostUSD: 0.42,
	}); err != nil {
		t.Fatalf("seed current-month conversation: %v", err)
	}
	// An old conversation outside the month must not count.
	if err := st.CreateConversation(ctx, "u1", &store.Conversation{
		SessionID: "old", TS: "2020-01-05T10:00:00Z", TurnCount: 4, CostUSD: 9.99,
	}); err != nil {
		t.Fatalf("seed old conversation: %v", err)
	}

	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/costs", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("costs status = %d (%v)", resp.StatusCode, body)
	}
	if usd, _ := body["totalUsd"].(float64); usd != 0.42 {
		t.Errorf("totalUsd = %v, want 0.42", body["totalUsd"])
	}
	if n, _ := body["conversations"].(float64); n != 1 {
		t.Errorf("conversations = %v, want 1", body["conversations"])
	}
	if n, _ := body["costed"].(float64); n != 1 {
		t.Errorf("costed = %v, want 1", body["costed"])
	}
}
