package sync

// User-scoped event fan-out over IoT Core (plan.md §6 WS-2).
//
// The shadow path above this pushes SETTINGS to a device's named shadow. This
// is the other direction of the same pipe: small notifications published to a
// per-USER topic, so every device that user has signed in — web, Android —
// learns immediately that a shared document, memory entity or plan changed.
//
// The payload is deliberately a NOTIFICATION, not the change itself. Clients
// already read live state through their own authenticated tool calls
// (file_read, memory_search), so shipping content here would duplicate a
// source of truth and put user data on a topic for no gain. What travels is
// enough to decide whether to care: what changed, which version, and who did
// it.
//
// actorDeviceId is the load-bearing field. Without it, the device that MADE a
// change is told about its own edit and announces it back to the user, which
// reads as the assistant talking to itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"
)

// Topic namespace. Device topics are `liveninja/<thingName>/...` (the IoT
// device policy in template.yaml scopes every action to that shape), so the
// user namespace lives one level in under a segment a Thing may never be
// named — see ReservedThingSegment.
const (
	topicRoot = "liveninja"
	// userSegment separates user topics from device topics. A Thing called
	// "user" would make `liveninja/user/...` ambiguous between the two
	// namespaces, which is why ReservedThingSegment exists.
	userSegment = "user"
	// speakingSegment is the turn-taking lock's own leaf. It hangs off the
	// presence prefix rather than sitting beside the event kinds, for a reason
	// that is entirely about rollout and not about taxonomy — see SpeakingTopic
	// before moving it.
	speakingSegment = "speaking"
)

// Event kinds. These name what changed, not how.
const (
	EventDoc      = "doc"      // a deliverable/file was created or edited
	EventMemory   = "memory"   // a memory entity or plan was written
	EventPresence = "presence" // a device joined/left (published by CLIENTS)
)

// ReservedThingSegment is the one Thing name that must never be provisioned:
// it would collide with the user-event namespace. Device topics are
// `liveninja/<thingName>/…` and user topics are `liveninja/user/…`, so a Thing
// literally named "user" would be able to publish onto — and subscribe to —
// every user's event stream through its own device policy.
const ReservedThingSegment = userSegment

// IsReservedThingName reports whether name would collide with the user-event
// topic namespace. Provisioning must refuse it.
func IsReservedThingName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), ReservedThingSegment)
}

// UserEventTopic is where server-authored events for one user are published.
// The IoT custom authorizer grants Subscribe on `liveninja/user/<uid>/#` and
// Receive on `liveninja/user/<uid>/*`, so every kind lands in one subscription.
func UserEventTopic(userID, kind string) string {
	return fmt.Sprintf("%s/%s/%s/%s", topicRoot, userSegment, userID, kind)
}

// PresenceTopic is the per-device presence slot. This is the ONLY prefix
// clients may publish to (cmd/iot-authorizer), and each client sets its MQTT
// Last Will here so a dropped connection clears itself without the server
// having to notice.
func PresenceTopic(userID, deviceID string) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", topicRoot, userSegment, userID, EventPresence, deviceID)
}

// SpeakingTopic is the per-USER turn-taking lock (plan.md §6 WS-5 M5.2).
//
// One topic for the whole account, not one per device: the lock is a single
// shared resource, and every device already receives it through the
// `liveninja/user/<uid>/#` subscription the authorizer grants. Every client
// takes the string from here (the web and Android clients are handed it by
// GET /api/v1/iot/credentials) — nobody concatenates their own copy, because a
// claim published one segment away from the grant is refused, and AWS signals
// a refused publish by closing the socket rather than by erroring.
//
// WHY THE LOCK LIVES UNDER presence/ — do NOT "tidy" this back to
// `liveninja/user/<uid>/speaking`. That shorter topic is what the first cut
// used, and moving it under presence/ is the whole reason this change can be
// deployed without every already-open browser tab misbehaving:
//
//   - A tab open ACROSS the deploy keeps running the OLD module graph, and the
//     old clients have no branch for a lock topic at all. On old web, a claim
//     payload parses as JSON, contains no actorDeviceId so the self-filter
//     misses it, and falls through to the nudge path — so the assistant says
//     "[Automatic update] Another device just changed something shared" for an
//     edit that never happened, on EVERY claim, until that tab is reloaded.
//   - Both old clients ignore any topic containing "/presence/": old web routes
//     it to a presence branch whose onPresence callback was never supplied (so
//     it is swallowed), and old Android returns early on
//     `pub.topic.contains("/presence/")`. Under this prefix an old tab drops
//     the claim instead of narrating it, which is what makes the rollout
//     silent.
//
// The price is that this topic is now byte-identical to
// PresenceTopic(userID, "speaking") — a peer whose device id is literally
// "speaking". NEW clients MUST therefore test for the lock topic BEFORE their
// presence branch, or a claim is filed into the roster as a phantom peer. That
// ordering is asserted on each client rather than left to reading order.
//
// Publish authority: the lock is covered by the authorizer's existing
// `topic/<user>/presence/*` publish grant and no longer has a statement of its
// own — see cmd/iot-authorizer, which also records why the lock being inside
// the RetainPublish grant is acceptable.
//
// "speaking" is still deliberately absent from the event-kind block above. A
// kind there is server-publishable through PublishEvent; the lock is
// client-only. Since the move, PublishEvent's existing refusal of
// EventPresence covers the lock too — the server cannot reach anything under
// presence/ — so the closed kind set and that one refusal make a server-side
// claim (which no client would ever release) structurally unreachable.
func SpeakingTopic(userID string) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", topicRoot, userSegment, userID, EventPresence, speakingSegment)
}

// Event is one fan-out notification.
type Event struct {
	// Type is an event kind (EventDoc, EventMemory).
	Type string `json:"type"`
	// ID identifies the thing that changed (deliverable id, entity id).
	ID string `json:"id,omitempty"`
	// Version is the changed object's new version where it has one; 0 when the
	// object carries no version, so clients must treat it as advisory.
	Version int64 `json:"version,omitempty"`
	// ActorDeviceID is the device that made the change. A client compares this
	// against its own id and IGNORES its own edits — without it, the device
	// that made a change announces that change back to the user.
	ActorDeviceID string `json:"actorDeviceId,omitempty"`
	// ActorPersona is the persona in use on the actor device, so another
	// device can say "the Staff SRE changed the plan" rather than "something
	// changed".
	ActorPersona string `json:"actorPersona,omitempty"`
	// Summary is one short human phrase, already safe to speak aloud.
	Summary string `json:"summary,omitempty"`
}

// PublishEvent fans one event out to every device signed in as userID.
//
// Errors are returned but callers are expected to LOG AND CONTINUE: this is a
// notification, and failing a user's file write because a convenience ping
// could not be delivered would be strictly worse than the ping being missed.
func (p *Publisher) PublishEvent(ctx context.Context, userID string, ev Event) error {
	if userID == "" {
		return errors.New("sync: userID is required")
	}
	if ev.Type == "" {
		return errors.New("sync: event type is required")
	}
	// Clients own presence; the server publishing it would fight the Last Will
	// that clears it.
	if ev.Type == EventPresence {
		return errors.New("sync: presence is client-published, not server-published")
	}

	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("sync: marshal event: %w", err)
	}
	client, err := p.client(ctx)
	if err != nil {
		return err
	}
	if _, err := client.Publish(ctx, &iotdataplane.PublishInput{
		Topic:   aws.String(UserEventTopic(userID, ev.Type)),
		Qos:     0, // fire-and-forget: a missed ping costs a stale panel, not data
		Payload: payload,
	}); err != nil {
		return fmt.Errorf("sync: publish %s event: %w", ev.Type, err)
	}
	return nil
}
