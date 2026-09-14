package webapp

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/agentmemory"
	"github.com/JeremyProffittOrg/live-ninja/internal/agentmemory/agentmemorytest"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

// agentcore-memory (plan.md event-writer + memory-tools-cutover): the
// transcript sink writes events only for admitted callers and never fails
// the flush; the remembered routes are role-gated and ownership-checked.

func newAgentMemoryApp(t *testing.T, role string, fake *agentmemorytest.Fake) (*fiber.App, *captureLambda) {
	t.Helper()
	t.Setenv("TOPICS_EXTRACT_FUNCTION_NAME", "topics-extract-test")
	st := store.NewWithClient(testutil.NewFakeDynamo(), "live-ninja")
	capture := &captureLambda{}
	deps := &Deps{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Lambda: capture}
	deps.AgentMemory = agentmemory.New(fake, agentmemory.Config{MemoryID: "mem-1", Mode: agentmemory.ModeOwner}, deps.Log)
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals(localUserID, "u1")
		c.Locals(localSurface, "web")
		c.Locals(localRole, role)
		return c.Next()
	})
	app.Post("/api/v1/transcript", handleTranscript(deps))
	app.Get("/api/v1/memory/remembered", handleListRemembered(deps))
	app.Delete("/api/v1/memory/remembered/:id", handleForgetRemembered(deps))
	return app, capture
}

func transcriptBody(sessionID string) map[string]any {
	return map[string]any{
		"sessionId": sessionID,
		"turns": []map[string]any{
			{"seq": 1, "role": "user", "text": "My sister is Sarah.", "engine": "openai-realtime"},
			{"seq": 2, "role": "assistant", "text": "Noted.", "engine": "openai-realtime"},
			{"seq": 3, "role": "user", "text": "Thanks", "engine": "openai-realtime"},
		},
	}
}

func TestTranscriptWritesOneAgentCoreEventPerExchangeForAdmittedCaller(t *testing.T) {
	fake := &agentmemorytest.Fake{}
	app, _ := newAgentMemoryApp(t, "owner", fake)

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/transcript", transcriptBody("sessA"))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.EqualValues(t, 3, body["written"])

	require.Len(t, fake.Events, 2, "user+assistant pair, then the lone user turn")
	assert.Equal(t, "u1", aws.ToString(fake.Events[0].ActorId))
	assert.Equal(t, "sessA", aws.ToString(fake.Events[0].SessionId))
	assert.Len(t, fake.Events[0].Payload, 2)
	assert.Len(t, fake.Events[1].Payload, 1)
}

func TestTranscriptSkipsAgentCoreOutsideTheRolloutMode(t *testing.T) {
	fake := &agentmemorytest.Fake{}
	app, _ := newAgentMemoryApp(t, "member", fake)

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/transcript", transcriptBody("sessB"))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.EqualValues(t, 3, body["written"], "the DynamoDB transcript is unaffected by the gate")
	assert.Empty(t, fake.Events, "mode=owner admits no member events")
}

func TestTranscriptFlushSurvivesAgentCoreFailure(t *testing.T) {
	fake := &agentmemorytest.Fake{CreateErr: errors.New("throttled")}
	app, _ := newAgentMemoryApp(t, "owner", fake)

	resp, body := doJSON(t, app, http.MethodPost, "/api/v1/transcript", transcriptBody("sessC"))
	require.Equal(t, http.StatusOK, resp.StatusCode, "a memory failure must never fail the flush")
	assert.EqualValues(t, 3, body["written"])
	assert.Empty(t, fake.Events)
}

func TestRememberedListIsOffOutsideTheRolloutMode(t *testing.T) {
	fake := &agentmemorytest.Fake{Records: []types.MemoryRecordSummary{agentmemorytest.Summary("r1", "/users/u1/facts/", "Sister: Sarah", 0.9)}}
	app, _ := newAgentMemoryApp(t, "member", fake)

	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/memory/remembered", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, false, body["enabled"])
	assert.Empty(t, body["items"])

	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/memory/remembered/r1", nil)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Empty(t, fake.Deleted)
}

func TestRememberedListAndForgetForAdmittedCaller(t *testing.T) {
	fake := &agentmemorytest.Fake{Records: []types.MemoryRecordSummary{
		agentmemorytest.Summary("r1", "/users/u1/facts/", "Sister: Sarah", 0.9),
	}}
	fake.Records = append(fake.Records, agentmemorytest.Summary("r2", "/users/u2/facts/", "Someone else's", 0.9))
	app, _ := newAgentMemoryApp(t, "owner", fake)

	resp, body := doJSON(t, app, http.MethodGet, "/api/v1/memory/remembered", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, true, body["enabled"])
	items, _ := body["items"].([]any)
	require.Len(t, items, 2, "the fake lists what it holds; the server filters by namespace at the API, not here")
	first, _ := items[0].(map[string]any)
	assert.Equal(t, "r1", first["id"])
	assert.Equal(t, "Sister: Sarah", first["text"])
	assert.Equal(t, "/users/u1/facts/", first["namespace"])

	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/memory/remembered/r2", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "another actor's record is not_found, never deleted")
	assert.Empty(t, fake.Deleted)

	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/memory/remembered/r1", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, []string{"r1"}, fake.Deleted)

	resp, _ = doJSON(t, app, http.MethodDelete, "/api/v1/memory/remembered/missing", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
