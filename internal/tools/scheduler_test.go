package tools

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// capturingScheduler is a minimal SchedulerAPI fake that records every
// CreateSchedule call so tests can assert on the computed fire time.
type capturingScheduler struct {
	calls []*scheduler.CreateScheduleInput
}

func (c *capturingScheduler) CreateSchedule(_ context.Context, in *scheduler.CreateScheduleInput, _ ...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error) {
	c.calls = append(c.calls, in)
	return &scheduler.CreateScheduleOutput{}, nil
}

// discardSQS is a no-op SQSAPI fake — set_timer/set_reminder enqueue a
// notification alongside the schedule; these tests don't care about it.
type discardSQS struct{}

func (discardSQS) SendMessage(_ context.Context, _ *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	return &sqs.SendMessageOutput{}, nil
}

// schedulerTestDeps wires the scheduler + SQS dependencies set_timer and
// set_reminder require, on a fixed clock so fire-time comparisons across
// separate Invoke calls are deterministic.
func schedulerTestDeps(fixedNow time.Time) (*Deps, *capturingScheduler) {
	deps := newTestDeps()
	sched := &capturingScheduler{}
	deps.Scheduler = sched
	deps.SchedulerGroup = "test-group"
	deps.SchedulerRoleARN = "arn:aws:iam::123456789012:role/test"
	deps.SQS = discardSQS{}
	deps.EmailQueueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/test-queue"
	deps.OwnerEmail = "owner@jeremy.ninja"
	deps.Now = func() time.Time { return fixedNow }
	return deps, sched
}

// TestResolveFireTimeSecondsAliasMatchesInSeconds is the A2-mandated
// regression: 'seconds' is a load-bearing prod-compat alias for 'inSeconds'
// (scheduler.go's minLeadSeconds/maxLead history — every set_timer call
// failed invalid_args in prod before this alias existed) and MUST resolve
// to an identical fire time, even though 'seconds' is no longer advertised
// in the schema (Q2).
func TestResolveFireTimeSecondsAliasMatchesInSeconds(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	viaInSeconds, err := resolveFireTime(now, map[string]any{"inSeconds": 3600}, time.UTC)
	require.Nil(t, err)

	viaSeconds, err := resolveFireTime(now, map[string]any{"seconds": 3600}, time.UTC)
	require.Nil(t, err)

	assert.Equal(t, viaInSeconds, viaSeconds, "inSeconds and seconds must resolve to an identical fire time")
	assert.Equal(t, now.Add(3600*time.Second), viaInSeconds)
}

// TestSetTimerManifestAdvertisesOnlyInSeconds is the Q2 lock: the model is
// taught exactly one spelling ("inSeconds"); "seconds" keeps working
// (validateArgs still declares it, see TestSetTimerAcceptsUnadvertisedSecondsAlias)
// but must never appear in the rendered schema — no synonym pair for the
// model to be confused by.
func TestSetTimerManifestAdvertisesOnlyInSeconds(t *testing.T) {
	r := newTestRegistry(t, newTestDeps())
	manifest := r.Manifest()

	var setTimer map[string]any
	for _, m := range manifest {
		if m["name"] == "set_timer" {
			setTimer = m
			break
		}
	}
	require.NotNil(t, setTimer, "set_timer must be in the manifest")

	params := setTimer["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	assert.Contains(t, props, "inSeconds")
	assert.NotContains(t, props, "seconds",
		"seconds must stay a hidden back-compat alias, never advertised to the model")

	inSeconds := props["inSeconds"].(map[string]any)
	assert.Equal(t, float64(86400), inSeconds["maximum"], "Q3: set_timer capped at 24h (86400s)")
}

// TestSetReminderManifestAdvertisesOnlyInSeconds extends the Q2 lock to the
// sibling tool: set_reminder's "seconds" alias must also stay a hidden
// back-compat spelling. Without this, the M19 manifest flip (which
// advertises set_reminder.inSeconds for the first time, per D-a) would
// teach the model the exact inSeconds/seconds synonym pair Q2 eliminated —
// just one tool over. The alias must still execute through the router.
func TestSetReminderManifestAdvertisesOnlyInSeconds(t *testing.T) {
	fixedNow := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	deps, sched := schedulerTestDeps(fixedNow)
	r := newTestRegistry(t, deps)

	var setReminder map[string]any
	for _, m := range r.Manifest() {
		if m["name"] == "set_reminder" {
			setReminder = m
			break
		}
	}
	require.NotNil(t, setReminder, "set_reminder must be in the manifest")

	props := setReminder["parameters"].(map[string]any)["properties"].(map[string]any)
	assert.Contains(t, props, "inSeconds")
	assert.NotContains(t, props, "seconds",
		"seconds must stay a hidden back-compat alias on set_reminder too, never advertised to the model")

	// The unadvertised alias still executes end to end.
	inv := invocation("set_reminder", map[string]any{
		"message": "stretch your legs",
		"seconds": float64(600),
	})
	inv.IdempotencyKey = "k-reminder-seconds"
	res := r.Invoke(context.Background(), inv)
	require.True(t, res.OK, "the unadvertised 'seconds' alias must still be accepted by set_reminder: %+v", res.Error)
	require.Len(t, sched.calls, 1)
}

// TestSetTimerAcceptsUnadvertisedSecondsAlias proves the alias still
// actually executes end to end through the router even though the schema
// no longer advertises it, and that it produces the exact same schedule
// as the equivalent inSeconds call.
func TestSetTimerAcceptsUnadvertisedSecondsAlias(t *testing.T) {
	fixedNow := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	deps, sched := schedulerTestDeps(fixedNow)
	r := newTestRegistry(t, deps)
	ctx := context.Background()

	invSeconds := invocation("set_timer", map[string]any{"seconds": float64(600)})
	invSeconds.IdempotencyKey = "k-seconds"
	res := r.Invoke(ctx, invSeconds)
	require.True(t, res.OK, "the unadvertised 'seconds' alias must still be accepted: %+v", res.Error)
	require.Len(t, sched.calls, 1)

	invInSeconds := invocation("set_timer", map[string]any{"inSeconds": float64(600)})
	invInSeconds.IdempotencyKey = "k-inseconds"
	res = r.Invoke(ctx, invInSeconds)
	require.True(t, res.OK, "inSeconds must be accepted: %+v", res.Error)
	require.Len(t, sched.calls, 2)

	require.NotNil(t, sched.calls[0].ScheduleExpression)
	require.NotNil(t, sched.calls[1].ScheduleExpression)
	assert.Equal(t, *sched.calls[0].ScheduleExpression, *sched.calls[1].ScheduleExpression,
		"seconds and inSeconds must produce an identical fire time end to end")
}

// TestSetTimerOverflowNamesSetReminder is the D-a second-pass decision:
// with the 24h cap, exceeding it must return an invalid_args error naming
// set_reminder so the model can self-correct ("set a timer for 3 days")
// instead of dead-ending. Both spellings must carry the same guidance.
func TestSetTimerOverflowNamesSetReminder(t *testing.T) {
	fixedNow := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	deps, _ := schedulerTestDeps(fixedNow)
	r := newTestRegistry(t, deps)
	ctx := context.Background()

	inv := invocation("set_timer", map[string]any{"inSeconds": float64(90000)}) // > 86400
	inv.IdempotencyKey = "k1"
	res := r.Invoke(ctx, inv)
	require.False(t, res.OK)
	assert.Equal(t, CodeInvalidArgs, res.Error.Code)
	assert.Contains(t, res.Error.Message, "set_reminder",
		"the overflow error must name set_reminder so the model can self-correct")

	inv2 := invocation("set_timer", map[string]any{"seconds": float64(90000)})
	inv2.IdempotencyKey = "k2"
	res2 := r.Invoke(ctx, inv2)
	require.False(t, res2.OK)
	assert.Contains(t, res2.Error.Message, "set_reminder",
		"the seconds alias must carry the same set_reminder guidance")
}

// TestSetReminderStillAllowsBeyondTimerCap confirms set_timer's new 24h
// cap (Q3) is tool-specific and does not narrow set_reminder's much wider
// ceiling — D-a's handoff only works if set_reminder can actually serve
// what set_timer now refuses.
func TestSetReminderStillAllowsBeyondTimerCap(t *testing.T) {
	fixedNow := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	deps, _ := schedulerTestDeps(fixedNow)
	r := newTestRegistry(t, deps)

	inv := invocation("set_reminder", map[string]any{
		"message":   "check the roast",
		"inSeconds": float64(90000), // > set_timer's 86400 cap
	})
	inv.IdempotencyKey = "k1"
	res := r.Invoke(context.Background(), inv)
	require.True(t, res.OK, "set_reminder must still serve durations beyond set_timer's 24h cap: %+v", res.Error)
}

// M15: the model now knows the local clock from its base knowledge, so it
// emits naive local datetimes ("2026-07-25T09:00:00") far more often than
// correctly-offset RFC3339. Before this those were a hard invalid_args.
func TestResolveFireTimeAcceptsNaiveLocalDatetime(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC) // 08:00 EDT

	cases := []struct {
		name string
		at   string
	}{
		{"seconds precision", "2026-07-25T09:00:00"},
		{"minute precision", "2026-07-25T09:00"},
		{"space separator", "2026-07-25 09:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, terr := resolveFireTime(now, map[string]any{"at": tc.at}, ny)
			require.Nil(t, terr)
			// 9am Eastern is 13:00 UTC — the point of the whole exercise.
			assert.Equal(t, time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC), got.UTC())
		})
	}
}

// An explicit offset still wins: a naive parse must never reinterpret a time
// the caller already pinned to a zone.
func TestResolveFireTimeOffsetTimeIsUnchanged(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	got, terr := resolveFireTime(now, map[string]any{"at": "2026-07-25T09:00:00-07:00"}, ny)
	require.Nil(t, terr)
	assert.Equal(t, time.Date(2026, 7, 25, 16, 0, 0, 0, time.UTC), got.UTC())
}

func TestResolveFireTimeStillRejectsGarbage(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	_, terr := resolveFireTime(now, map[string]any{"at": "next tuesday-ish"}, time.UTC)
	require.NotNil(t, terr)
	assert.Equal(t, CodeInvalidArgs, terr.Code)
}

// schedulerLocation degrades to UTC rather than failing when the profile
// carries no timezone or a stale one.
func TestSchedulerLocationDegrades(t *testing.T) {
	assert.Equal(t, time.UTC, schedulerLocation(store.Profile{}))
	assert.Equal(t, time.UTC, schedulerLocation(store.Profile{
		HomeLocation: &store.Location{Label: "x", Lat: 1, Lon: 2, Timezone: "Mars/Olympus_Mons"},
	}))
	loc := schedulerLocation(store.Profile{
		HomeLocation: &store.Location{Label: "x", Lat: 1, Lon: 2, Timezone: "America/New_York"},
	})
	assert.Equal(t, "America/New_York", loc.String())
}
