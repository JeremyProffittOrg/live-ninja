package rca

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

func TestSubjectFormat(t *testing.T) {
	assert.Equal(t,
		"Live Ninja RCA: get_weather — the model sent a placeholder location",
		Subject("get_weather", "the model sent a placeholder location", "invalid_args"))

	// An empty symptom falls back to the error code rather than a bare tool name.
	assert.Equal(t,
		"Live Ninja RCA: get_weather — invalid_args",
		Subject("get_weather", "   ", "invalid_args"))

	// Newlines/tabs collapse — a subject header cannot contain them.
	assert.Equal(t,
		"Live Ninja RCA: get_weather — a b",
		Subject("get_weather", "a\n\tb", "invalid_args"))

	// A 300-rune symptom is capped with an ellipsis, and the whole subject is
	// capped too.
	long := Subject("get_weather", strings.Repeat("x", 300), "invalid_args")
	assert.True(t, strings.HasSuffix(long, "…"))
	assert.LessOrEqual(t, len([]rune(long)), maxSubjectRunes)
	assert.LessOrEqual(t, len([]rune(long))-len([]rune("Live Ninja RCA: get_weather — ")), maxSubjectSymptomRunes)

	assert.Contains(t, Subject("get_weather", "x", "invalid_args"), emDash)
}

// goldenEmail builds the fixed record/report/input triple the body snapshot pins.
func goldenEmail() (store.RCARecord, Report, PromptInput, []string, Config) {
	in := goldenPromptInput()
	rep := Report{
		Symptom:    "get_weather was called with a one-character location placeholder",
		RootCause:  "The model emitted \"x\" for the location argument instead of omitting it, so the router's MinLen=2 gate rejected the call before internal/tools/weather.go could fall back to the profile's stored home coordinates.",
		Evidence:   []string{`the audit turn shows args={"location":"x"}`, "the profile has a geocode-resolved home location"},
		Confidence: ConfidenceHigh,
		CodeFixSuggestions: []string{
			"internal/tools/weather.go: describe in the location param that omitting it uses the profile home location",
			"internal/realtime/baseknowledge.go: state the stored home location explicitly in BASE KNOWLEDGE",
		},
		BaseKnowledgeSuggestions: []ReportSuggestion{
			{Field: FieldProfileUnits, ProposedValue: "metric", Reason: "the user asked for celsius twice"},
		},
		ReproSteps: []string{`POST /api/v1/tools/invoke with {"tool":"get_weather","args":{"location":"x"}}`},
	}
	rec := store.RCARecord{
		PK:              Family("get_weather", "invalid_args"),
		RCAID:           "aa11bb22cc33",
		Status:          store.RCAStatusAnalyzed,
		Tool:            "get_weather",
		ErrorCode:       "invalid_args",
		Signature:       "0123456789abcdef",
		ErrorMessage:    `argument "location" must be at least 2 characters`,
		ArgsJSON:        `{"location":"x"}`,
		TxID:            "8f2c1d6e-0000-4000-8000-000000000001",
		CallID:          "call_abc",
		UserID:          "u-golden",
		SessionID:       "sess-123",
		Surface:         "web",
		Engine:          "gpt-realtime",
		TurnsInWindow:   14,
		OccurredAt:      "2026-07-25T14:03:11.482913Z",
		CreatedAt:       "2026-07-25T14:03:12.000000Z",
		ModelID:         "us.anthropic.claude-opus-4-5-20251101-v1:0",
		PromptSHA256:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		StopReason:      "end_turn",
		InputTokens:     6412,
		OutputTokens:    744,
		Confidence:      ConfidenceHigh,
		SuppressedCount: 3,
		SuggestionIDs:   []string{"ff00aa11bb22"},
	}
	cfg := Config{
		ModelID:      rec.ModelID,
		DailyCap:     10,
		Cooldown:     defaultCooldown,
		NoticeWindow: noticeWindow,
	}
	return rec, rep, in, nil, cfg
}

func TestBodyGolden(t *testing.T) {
	rec, rep, in, notes, cfg := goldenEmail()
	got := Body(rec, rep, in, notes, cfg)
	assertGolden(t, goldenEmailPath, got,
		"review the report layout change, then re-record with "+
			"`go test ./internal/rca -run TestBodyGolden -update`")
}

func TestBodyOmitsEmptySections(t *testing.T) {
	rec, _, in, _, cfg := goldenEmail()
	rec.SuggestionIDs = nil
	rep := Report{
		Symptom:    "something went wrong",
		RootCause:  "not enough evidence to say",
		Confidence: ConfidenceLow,
	}
	body := Body(rec, rep, in, nil, cfg)

	assert.NotContains(t, body, "\nEVIDENCE\n")
	assert.NotContains(t, body, "\nSUGGESTED CODE FIXES\n")
	assert.NotContains(t, body, "\nREPRO STEPS\n")
	assert.NotContains(t, body, "\nNOTES\n")

	// The suggestions section is always present so "none proposed" is explicit
	// rather than indistinguishable from a rendering bug.
	assert.Contains(t, body, "BASE-KNOWLEDGE SUGGESTIONS")
	assert.Contains(t, body, "  (none)\n")

	// And the always-on sections survive.
	assert.Contains(t, body, "\nSYMPTOM\n")
	assert.Contains(t, body, "\nCONTEXT\n")
	assert.Contains(t, body, "\nMODEL\n")
	assert.True(t, strings.HasSuffix(body, "per UTC day.\n"))
}

func TestBodyIncludesNotesAndSuggestionIDs(t *testing.T) {
	rec, rep, in, _, cfg := goldenEmail()
	notes := []string{"NOTE: the model response hit the output-token limit (RCA_MAX_OUTPUT_TOKENS)."}
	body := Body(rec, rep, in, notes, cfg)

	assert.Contains(t, body, "\nNOTES\n")
	assert.Contains(t, body, "output-token limit")
	assert.Contains(t, body, "[PROFSUGG id ff00aa11bb22]")
	assert.Contains(t, body, "profile.units = metric")
}

// TestBodyFooterReflectsConfig guards against a footer that misstates the
// dedupe policy after the stack parameters change.
func TestBodyFooterReflectsConfig(t *testing.T) {
	rec, rep, in, notes, cfg := goldenEmail()
	assert.Contains(t, Body(rec, rep, in, notes, cfg), "1 analysis per hour per signature, 10 per UTC day.")

	cfg.DailyCap = 3
	cfg.Cooldown = defaultCooldown * 2
	assert.Contains(t, Body(rec, rep, in, notes, cfg), "1 analysis per 2h per signature, 3 per UTC day.")
}

func TestNoticeBodiesNameTheRemediation(t *testing.T) {
	cfg := Config{ModelID: "us.anthropic.claude-opus-4-5-20251101-v1:0", NoticeWindow: noticeWindow}

	subject, body := modelUnavailableNotice(cfg, "AccessDeniedException")
	assert.Equal(t, "Live Ninja RCA: disabled — Bedrock model access unavailable", subject)
	assert.Contains(t, body, cfg.ModelID)
	assert.Contains(t, body, "AccessDeniedException")
	assert.Contains(t, body, "Model access")
	assert.Contains(t, body, "status=model_unavailable")
	assert.Contains(t, body, "once per 24h")

	subject, body = malformedResponseNotice(cfg, "aa11bb22cc33", "RCA#get_weather#invalid_args", "rca: no json")
	assert.Contains(t, subject, "not parseable")
	assert.Contains(t, body, "aa11bb22cc33")
	assert.Contains(t, body, "RCA#get_weather#invalid_args")
	assert.Contains(t, body, "rawResponse")
}

func TestIndentWrappedKeepsLongTokensIntact(t *testing.T) {
	long := "internal/tools/weather.go:resolveLocationFromProfileWithAVeryLongSymbolNameThatExceedsTheWrapColumnAllByItself"
	out := indentWrapped(long)
	assert.Contains(t, out, long, "a split symbol name would be unsearchable")

	wrapped := indentWrapped(strings.Repeat("word ", 60))
	for _, line := range strings.Split(strings.TrimRight(wrapped, "\n"), "\n") {
		assert.True(t, strings.HasPrefix(line, "  "), "every wrapped line keeps the 2-space indent")
		assert.LessOrEqual(t, len(line), bodyWrapColumns)
	}
}
