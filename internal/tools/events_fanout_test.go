package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lnsync "github.com/JeremyProffittOrg/live-ninja/internal/sync"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeEvents records the change fan-out and can fail on demand.
type fakeEvents struct {
	sent []lnsync.Event
	err  error
}

func (f *fakeEvents) PublishEvent(ctx context.Context, userID string, ev lnsync.Event) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, ev)
	return nil
}

// TestChangeEventKindIsAnAllowlist: silence is the default. A new tool must
// opt IN to waking every other device the user owns, so this map is the whole
// list and nothing else may publish.
func TestChangeEventKindIsAnAllowlist(t *testing.T) {
	assert.Equal(t, lnsync.EventDoc, changeEventKind["deliverable_create"])
	assert.Equal(t, lnsync.EventDoc, changeEventKind["file_create"])
	assert.Equal(t, lnsync.EventMemory, changeEventKind["memory_write"])
	assert.Equal(t, lnsync.EventMemory, changeEventKind["plan_upsert"])
	assert.Len(t, changeEventKind, 4, "adding a tool here wakes every device — do it deliberately")

	// Read-only and unrelated tools must never announce anything.
	for _, tool := range []string{
		"memory_search", "file_read", "file_list", "entity_get",
		"get_weather", "send_email", "web_lookup", "set_timer",
	} {
		_, present := changeEventKind[tool]
		assert.False(t, present, "%s must not fan out", tool)
	}
}

// TestPublishChangeCarriesTheActorDevice — without actorDeviceId the device
// that MADE a change is told about its own edit and announces it back to the
// user, which reads as the assistant talking to itself.
func TestPublishChangeCarriesTheActorDevice(t *testing.T) {
	ev := &fakeEvents{}
	r := &Registry{deps: &Deps{Events: ev, Log: testLog()}}

	r.publishChange(context.Background(), testLog(),
		Invocation{Tool: "file_create", UserID: "u1", DeviceID: "dev-web"},
		map[string]any{"fileId": "f-42"})

	require.Len(t, ev.sent, 1)
	assert.Equal(t, lnsync.EventDoc, ev.sent[0].Type)
	assert.Equal(t, "f-42", ev.sent[0].ID)
	assert.Equal(t, "dev-web", ev.sent[0].ActorDeviceID)
	assert.NotEmpty(t, ev.sent[0].Summary)
}

// TestPublishChangeNeverFailsTheCall: the write already succeeded and was
// already reported to the model. Turning a missed ping into a tool error would
// tell the user their file was not created when it was.
func TestPublishChangeNeverFailsTheCall(t *testing.T) {
	r := &Registry{deps: &Deps{Events: &fakeEvents{err: errors.New("iot down")}, Log: testLog()}}
	assert.NotPanics(t, func() {
		r.publishChange(context.Background(), testLog(),
			Invocation{Tool: "file_create", UserID: "u1"}, map[string]any{"fileId": "f"})
	})
}

// TestPublishChangeIsInertWithoutConfiguration — nil Events is the default in
// every environment that has not enabled the fan-out.
func TestPublishChangeIsInertWithoutConfiguration(t *testing.T) {
	r := &Registry{deps: &Deps{Log: testLog()}}
	assert.NotPanics(t, func() {
		r.publishChange(context.Background(), testLog(),
			Invocation{Tool: "file_create", UserID: "u1"}, map[string]any{"fileId": "f"})
	})

	// And no user context means no fan-out, ever.
	ev := &fakeEvents{}
	r2 := &Registry{deps: &Deps{Events: ev, Log: testLog()}}
	r2.publishChange(context.Background(), testLog(),
		Invocation{Tool: "file_create", UserID: ""}, map[string]any{"fileId": "f"})
	assert.Empty(t, ev.sent)
}

func TestFirstStringPrefersTheFirstPresentKey(t *testing.T) {
	out := map[string]any{"deliverableId": "d-1", "id": ""}
	assert.Equal(t, "d-1", firstString(out, "id", "fileId", "deliverableId"))
	assert.Equal(t, "", firstString(map[string]any{}, "id"))
	assert.Equal(t, "", firstString(map[string]any{"id": 7}, "id"), "non-strings are ignored")
}
