package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

// set_timer / set_reminder create a one-shot EventBridge Scheduler
// schedule (group SCHEDULER_GROUP) whose target is the EmailQueue via
// SCHEDULER_ROLE_ARN (scheduler->sqs). When it fires, email-dispatch
// delivers the notification to the owner's inbox — a real, durable
// serverless alarm with no polling infrastructure. The schedule deletes
// itself after completion (ActionAfterCompletion DELETE).
const (
	emailTemplateReminder = "tool-reminder"

	minLeadSeconds = 5                    // schedule must be at least this far out
	maxLead        = 366 * 24 * time.Hour // and at most ~1 year (set_reminder's ceiling)

	// maxTimerLead is set_timer's own, tighter ceiling (Q3, locked decision):
	// 24h. Anything longer is set_reminder's job — timerOverflowHint below
	// is what tells the model that when it hits this bound.
	maxTimerLead = 24 * time.Hour
)

// timerOverflowHint is appended to set_timer's out-of-range invalid_args
// error (D-a, second-pass decision): it must name set_reminder by name so
// the model can self-correct conversationally — e.g. "set a timer for 3
// days" — instead of just apologising that the call failed.
const timerOverflowHint = "For durations longer than 24 hours, use set_reminder (with inSeconds) instead."

func setTimerDefinition() *Definition {
	return &Definition{
		Name: "set_timer",
		Description: "Set a short one-shot timer that notifies the user when it fires. Provide the " +
			"duration in seconds (e.g. 600 for 10 minutes). Capped at 24 hours (86400 seconds) — " +
			"for anything longer, use set_reminder instead.",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "inSeconds", Type: "integer", Min: floatPtr(minLeadSeconds), Max: floatPtr(maxTimerLead.Seconds()),
				OutOfRangeHint: timerOverflowHint,
				Description:    "How many seconds from now the timer should fire (max 86400 = 24h)."},
			// "seconds" is an accepted alias: models routinely send it
			// despite the schema naming "inSeconds" (observed in prod —
			// every set_timer call failed invalid_args). Per Q2 (locked
			// decision) it is deliberately NOT advertised — no synonym pair
			// taught to the model — but resolveFireTime below still accepts
			// it, so a model that sends it anyway (or a client still tuned
			// to the old manifest wording) keeps working exactly as before.
			{Name: "seconds", Type: "integer", Min: floatPtr(minLeadSeconds), Max: floatPtr(maxTimerLead.Seconds()),
				Unadvertised:   true,
				OutOfRangeHint: timerOverflowHint,
				Description:    "Alias for inSeconds."},
			{Name: "label", Type: "string", MaxLen: 200,
				Description: "Short label for what the timer is for, e.g. 'pasta on the stove'."},
		},
		Handler: func(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
			return handleSchedule(ctx, deps, inv, args, "Timer")
		},
	}
}

func setReminderDefinition() *Definition {
	return &Definition{
		Name: "set_reminder",
		Description: "Set a reminder for a specific date and time; the user is notified by email when it is due. " +
			"Provide either an absolute RFC3339 time in 'at' or a relative offset in 'inSeconds' (exactly one).",
		SideEffecting: true,
		Params: []ParamSpec{
			{Name: "message", Type: "string", Required: true, MinLen: 1, MaxLen: 500,
				Description: "What to remind the user about."},
			{Name: "at", Type: "string", MaxLen: 35,
				Description: "Absolute fire time, RFC3339 with offset, e.g. 2026-07-18T09:00:00-04:00."},
			{Name: "inSeconds", Type: "integer", Min: floatPtr(minLeadSeconds), Max: floatPtr(maxLead.Seconds()),
				Description: "Relative fire time: seconds from now. Use instead of 'at', not together."},
			// Same Q2 rationale as set_timer's alias above: "seconds" stays
			// accepted by resolveFireTime forever, but is never advertised —
			// otherwise the M19 manifest flip would teach the model the
			// exact inSeconds/seconds synonym pair Q2 was locked to
			// eliminate, just on the sibling tool.
			{Name: "seconds", Type: "integer", Min: floatPtr(minLeadSeconds), Max: floatPtr(maxLead.Seconds()),
				Unadvertised: true,
				Description:  "Alias for inSeconds."},
		},
		Handler: func(ctx context.Context, deps *Deps, inv Invocation, args map[string]any) (map[string]any, *ToolError) {
			return handleSchedule(ctx, deps, inv, args, "Reminder")
		},
	}
}

func handleSchedule(ctx context.Context, deps *Deps, inv Invocation, args map[string]any, kind string) (map[string]any, *ToolError) {
	if deps.Scheduler == nil || deps.SchedulerGroup == "" || deps.SchedulerRoleARN == "" {
		return nil, toolErrf(CodeNotConfigured, "scheduler is not configured")
	}
	if deps.SQS == nil || deps.EmailQueueURL == "" || deps.OwnerEmail == "" {
		return nil, toolErrf(CodeNotConfigured, "notification delivery is not configured")
	}
	queueARN, err := queueARNFromURL(deps.EmailQueueURL)
	if err != nil {
		deps.Log.Error("tools: bad email queue url", "error", err.Error())
		return nil, toolErrf(CodeNotConfigured, "notification queue is misconfigured")
	}

	now := deps.Now().UTC()
	// The user's timezone (M15): lets an 'at' value arrive as a naive local
	// datetime. The model now knows the local clock from its base knowledge,
	// so "remind me at 9am tomorrow" produces 2026-07-25T09:00:00 far more
	// often than a correctly-offset RFC3339 string — before this that was a
	// hard invalid_args, now it means 9am where the user actually is.
	fireAt, terr := resolveFireTime(now, args, schedulerLocation(deps.profileFor(ctx, inv.UserID)))
	if terr != nil {
		return nil, terr
	}

	// Notification content.
	message := kind
	if v, ok := args["message"].(string); ok && v != "" {
		message = v
	} else if v, ok := args["label"].(string); ok && v != "" {
		message = v
	}
	subject := fmt.Sprintf("[Live Ninja] %s: %s", kind, truncate(message, 120))
	text := fmt.Sprintf("%s set on %s via Live Ninja is due now (%s).\n\n%s",
		kind, now.Format(time.RFC1123), fireAt.Format(time.RFC1123), message)

	input, _ := json.Marshal(emailQueueMessage{
		Template: emailTemplateReminder,
		To:       deps.OwnerEmail,
		Subject:  subject,
		Text:     text,
	})

	name := scheduleName(inv.UserID, now)
	_, err = deps.Scheduler.CreateSchedule(ctx, &scheduler.CreateScheduleInput{
		Name:      aws.String(name),
		GroupName: aws.String(deps.SchedulerGroup),
		// One-shot at() expressions take a timezone-less local datetime
		// interpreted in ScheduleExpressionTimezone.
		ScheduleExpression:         aws.String("at(" + fireAt.UTC().Format("2006-01-02T15:04:05") + ")"),
		ScheduleExpressionTimezone: aws.String("UTC"),
		FlexibleTimeWindow: &schedulertypes.FlexibleTimeWindow{
			Mode: schedulertypes.FlexibleTimeWindowModeOff,
		},
		ActionAfterCompletion: schedulertypes.ActionAfterCompletionDelete,
		Target: &schedulertypes.Target{
			Arn:     aws.String(queueARN),
			RoleArn: aws.String(deps.SchedulerRoleARN),
			Input:   aws.String(string(input)),
			RetryPolicy: &schedulertypes.RetryPolicy{
				MaximumRetryAttempts: aws.Int32(3),
			},
		},
	})
	if err != nil {
		deps.Log.Error("tools: create schedule failed", "error", err.Error())
		return nil, toolErrf(CodeUpstreamError, "failed to create the %s", strings.ToLower(kind))
	}

	return map[string]any{
		"status":    "scheduled",
		"kind":      strings.ToLower(kind),
		"fireAt":    fireAt.UTC().Format(time.RFC3339),
		"inSeconds": int(fireAt.Sub(now).Seconds()),
		"name":      name,
	}, nil
}

// schedulerLocation resolves the profile timezone for naive 'at' values,
// degrading to UTC when the profile carries no timezone or names an unknown
// one (a stale zone id must never fail a reminder).
func schedulerLocation(p store.Profile) *time.Location {
	tz := p.Timezone()
	if tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

// naiveLocalLayouts are the offset-less datetime shapes accepted for 'at'
// and interpreted in the user's own timezone.
var naiveLocalLayouts = []string{
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
}

// resolveFireTime picks the fire time from 'at' xor 'inSeconds' and
// bounds it to [now+minLead, now+maxLead]. local interprets an 'at' value
// that carries no UTC offset.
func resolveFireTime(now time.Time, args map[string]any, local *time.Location) (time.Time, *ToolError) {
	atStr, hasAt := args["at"].(string)
	hasAt = hasAt && atStr != ""
	secs, hasSecs := args["inSeconds"].(int)
	if !hasSecs {
		secs, hasSecs = args["seconds"].(int) // accepted alias
	}

	switch {
	case hasAt && hasSecs:
		return time.Time{}, toolErrf(CodeInvalidArgs, "provide either 'at' or 'inSeconds', not both")
	case !hasAt && !hasSecs:
		return time.Time{}, toolErrf(CodeInvalidArgs, "provide 'at' (RFC3339) or 'inSeconds' (or 'seconds')")
	}

	var fireAt time.Time
	if hasAt {
		t, err := time.Parse(time.RFC3339, atStr)
		if err != nil {
			// Second chance: a naive local datetime, interpreted in the
			// user's timezone rather than rejected.
			parsed, ok := parseNaiveLocal(atStr, local)
			if !ok {
				return time.Time{}, toolErrf(CodeInvalidArgs,
					"'at' must be RFC3339 with an offset (2026-07-18T09:00:00-04:00) "+
						"or a local datetime (2026-07-18T09:00)")
			}
			t = parsed
		}
		fireAt = t
	} else {
		fireAt = now.Add(time.Duration(secs) * time.Second)
	}

	if fireAt.Before(now.Add(minLeadSeconds * time.Second)) {
		return time.Time{}, toolErrf(CodeInvalidArgs, "the requested time is in the past (or under %ds away)", minLeadSeconds)
	}
	if fireAt.After(now.Add(maxLead)) {
		return time.Time{}, toolErrf(CodeInvalidArgs, "the requested time is more than a year away")
	}
	return fireAt, nil
}

// parseNaiveLocal parses an offset-less datetime in loc.
func parseNaiveLocal(s string, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.UTC
	}
	for _, layout := range naiveLocalLayouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// scheduleName builds a unique EventBridge Scheduler name (allowed chars
// [0-9a-zA-Z-_.], max 64): ln-<uid8>-<unixms>-<rand4>.
func scheduleName(userID string, now time.Time) string {
	uid := sanitizeNameFragment(userID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	if uid == "" {
		uid = "anon"
	}
	return fmt.Sprintf("ln-%s-%d-%s", uid, now.UnixMilli(), randHex(2))
}

func sanitizeNameFragment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// queueARNFromURL derives an SQS queue ARN from its URL
// (https://sqs.<region>.amazonaws.com/<account>/<name>), so the template
// only has to pass EMAIL_QUEUE_URL once.
func queueARNFromURL(queueURL string) (string, error) {
	u, err := url.Parse(queueURL)
	if err != nil {
		return "", fmt.Errorf("parse queue url: %w", err)
	}
	hostParts := strings.Split(u.Host, ".")
	// sqs.<region>.amazonaws.com
	if len(hostParts) < 4 || hostParts[0] != "sqs" {
		return "", fmt.Errorf("unrecognized sqs host %q", u.Host)
	}
	region := hostParts[1]
	pathParts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(pathParts) != 2 || pathParts[0] == "" || pathParts[1] == "" {
		return "", fmt.Errorf("unrecognized sqs path %q", u.Path)
	}
	return fmt.Sprintf("arn:aws:sqs:%s:%s:%s", region, pathParts[0], pathParts[1]), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func randHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
