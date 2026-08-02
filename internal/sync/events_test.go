package sync

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTopicNamespace pins the topic shapes the IoT policy in
// cmd/iot-authorizer grants against. If these drift, the authorizer keeps
// granting the OLD shape and every client silently receives nothing.
func TestTopicNamespace(t *testing.T) {
	assert.Equal(t, "liveninja/user/u1/doc", UserEventTopic("u1", EventDoc))
	assert.Equal(t, "liveninja/user/u1/memory", UserEventTopic("u1", EventMemory))
	assert.Equal(t, "liveninja/user/u1/presence/dev-9", PresenceTopic("u1", "dev-9"))
	// The lock deliberately sits UNDER presence/, not beside it — see
	// SpeakingTopic. Pinned as a literal because the prefix is the entire
	// rollout mechanism: old clients drop "/presence/" topics, and a tidy-up
	// back to liveninja/user/u1/speaking would make every open tab announce a
	// phantom change on every claim.
	assert.Equal(t, "liveninja/user/u1/presence/speaking", SpeakingTopic("u1"))
	assert.Contains(t, SpeakingTopic("u1"), "/presence/",
		"old clients drop only topics containing '/presence/'")

	// The authorizer grants Subscribe on liveninja/user/<uid>/# — every topic
	// this package produces must actually sit under that filter.
	for _, topic := range []string{
		UserEventTopic("u1", EventDoc),
		UserEventTopic("u1", EventMemory),
		PresenceTopic("u1", "dev-9"),
		SpeakingTopic("u1"),
	} {
		assert.Regexp(t, `^liveninja/user/u1/`, topic)
	}
}

// TestSpeakingLockLooksExactlyLikeAPeerNamedSpeaking is the cost of putting the
// lock under presence/, stated so no client can claim it was not warned.
//
// SpeakingTopic(uid) and PresenceTopic(uid, "speaking") are the SAME string. A
// client that checks its presence branch first therefore files a turn-taking
// claim into its roster as a peer whose device id is literally "speaking" — a
// ghost device that never leaves, because nothing ever publishes a Last Will
// for it. Every client MUST test for the lock topic BEFORE the presence branch,
// and each one asserts that ordering in its own suite.
func TestSpeakingLockLooksExactlyLikeAPeerNamedSpeaking(t *testing.T) {
	assert.Equal(t, PresenceTopic("u1", "speaking"), SpeakingTopic("u1"),
		"clients must match the lock topic before their presence branch")
}

// TestSpeakingIsNotAServerEventKind pins the reason "speaking" is missing from
// the event-kind const block, which is otherwise the kind of omission a later
// reader tidies up. A kind there is server-publishable through PublishEvent,
// and a server-side claim is one no client would ever release — every device
// silenced for the length of its expiry.
//
// Since the lock moved under presence/, PublishEvent's existing refusal of
// EventPresence covers it as well: the server cannot address anything below
// that segment. The closed kind set and that one refusal together make a
// server-published claim unreachable rather than merely catchable.
func TestSpeakingIsNotAServerEventKind(t *testing.T) {
	for _, kind := range []string{EventDoc, EventMemory, EventPresence} {
		assert.NotEqual(t, "speaking", kind,
			"the turn-taking lock must never become a server-publishable kind")
	}

	// The lock sits under the one kind PublishEvent already refuses outright.
	assert.True(t, strings.HasPrefix(SpeakingTopic("u1"), UserEventTopic("u1", EventPresence)+"/"))

	// And today the server genuinely cannot reach it: the only publisher takes
	// a kind, "speaking" is not one, and presence is refused.
	fake := &fakeIoT{}
	p := NewWithClient(fake, nil)
	require.NoError(t, p.PublishEvent(context.Background(), "u1", Event{Type: EventDoc, ID: "d1"}))
	require.Error(t, p.PublishEvent(context.Background(), "u1", Event{Type: EventPresence}))
	for _, pub := range fake.published {
		assert.NotEqual(t, SpeakingTopic("u1"), pub.Topic)
	}
}

// TestReservedThingNameGuard: device topics are liveninja/<thingName>/… and
// user topics are liveninja/user/…, so a Thing named "user" would be able to
// publish onto and subscribe to every user's event stream using nothing but
// its own device policy. Provisioning must refuse that name.
func TestReservedThingNameGuard(t *testing.T) {
	for _, name := range []string{"user", "User", "USER", "  user  "} {
		assert.True(t, IsReservedThingName(name), "%q must be refused", name)
	}
	for _, name := range []string{"", "user-1", "tab5-abc", "users", "myuser"} {
		assert.False(t, IsReservedThingName(name), "%q is fine", name)
	}
}

func TestPublishEventFansOutToTheUserTopic(t *testing.T) {
	fake := &fakeIoT{}
	p := NewWithClient(fake, nil)

	err := p.PublishEvent(context.Background(), "u1", Event{
		Type:          EventDoc,
		ID:            "deliv-7",
		Version:       3,
		ActorDeviceID: "dev-web",
		ActorPersona:  "Staff SRE",
		Summary:       "updated the launch plan",
	})
	require.NoError(t, err)

	require.Len(t, fake.published, 1)
	assert.Equal(t, "liveninja/user/u1/doc", fake.published[0].Topic)

	var got Event
	require.NoError(t, json.Unmarshal(fake.published[0].Payload, &got))
	assert.Equal(t, "deliv-7", got.ID)
	assert.Equal(t, int64(3), got.Version)
	// The field that stops a device announcing its own edit back to the user.
	assert.Equal(t, "dev-web", got.ActorDeviceID)
	assert.Equal(t, "Staff SRE", got.ActorPersona)
}

// TestPublishEventCarriesNoContent: clients read live state through their own
// authenticated tool calls, so putting document text on a topic would
// duplicate a source of truth and expose user content for no gain.
func TestPublishEventCarriesNoContent(t *testing.T) {
	fake := &fakeIoT{}
	p := NewWithClient(fake, nil)
	require.NoError(t, p.PublishEvent(context.Background(), "u1", Event{
		Type: EventDoc, ID: "d1", Summary: "changed",
	}))

	var raw map[string]any
	require.NoError(t, json.Unmarshal(fake.published[0].Payload, &raw))
	for _, forbidden := range []string{"content", "body", "text", "instructions"} {
		_, present := raw[forbidden]
		assert.False(t, present, "event must not carry %q", forbidden)
	}
}

func TestPublishEventRejectsBadInput(t *testing.T) {
	p := NewWithClient(&fakeIoT{}, nil)
	ctx := context.Background()

	assert.Error(t, p.PublishEvent(ctx, "", Event{Type: EventDoc}))
	assert.Error(t, p.PublishEvent(ctx, "u1", Event{}))
	// Presence is client-published with an MQTT Last Will that clears it; the
	// server publishing it would fight that.
	assert.Error(t, p.PublishEvent(ctx, "u1", Event{Type: EventPresence}))
}

// TestPublishEventSurfacesTransportErrors — callers log and continue, but they
// can only do that if the error actually reaches them.
func TestPublishEventSurfacesTransportErrors(t *testing.T) {
	fake := &fakeIoT{publishErr: errors.New("iot unavailable")}
	p := NewWithClient(fake, nil)
	err := p.PublishEvent(context.Background(), "u1", Event{Type: EventMemory, ID: "e1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "iot unavailable")
}
