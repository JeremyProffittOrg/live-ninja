package rca

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

// baseFailure is the canonical invalid_args failure the signature tests vary.
func baseFailure() tools.ToolFailure {
	return tools.ToolFailure{
		V:            1,
		Source:       "tool-router",
		Tool:         "get_weather",
		ErrorCode:    tools.CodeInvalidArgs,
		ErrorMessage: `argument "location" must be at least 2 characters`,
		ArgsJSON:     `{"location":"x"}`,
		CallID:       "call_abc",
		TxID:         "8f2c1d6e-0000-4000-8000-000000000001",
		UserID:       "u1",
		SessionID:    "sess-123",
		Surface:      "web",
		Role:         "owner",
		OccurredAt:   "2026-07-25T14:03:11.482913Z",
	}
}

// TestSignatureStableAcrossCallers is the property the whole dedupe rests on:
// the same defect seen by a different user, session, call or minute is the SAME
// failure. Including any per-call identifier would make every failure unique and
// the cooldown a no-op.
func TestSignatureStableAcrossCallers(t *testing.T) {
	a := baseFailure()
	b := baseFailure()
	b.UserID = "u2"
	b.SessionID = "sess-999"
	b.CallID = "call_zzz"
	b.TxID = "aaaaaaaa-0000-4000-8000-000000000002"
	b.OccurredAt = "2026-07-26T09:00:00.000000Z"
	b.Surface = "android"
	b.Role = "member"

	assert.Equal(t, Signature(a), Signature(b))
}

func TestSignatureDistinguishesArgKeySets(t *testing.T) {
	a := baseFailure()
	a.ArgsJSON = `{"location":"x"}`
	b := baseFailure()
	b.ArgsJSON = `{"location":"x","units":"metric"}`

	assert.NotEqual(t, Signature(a), Signature(b),
		"a different argument key set is a different prompt/schema bug")
}

func TestSignatureIgnoresArgValues(t *testing.T) {
	a := baseFailure()
	a.ArgsJSON = `{"location":"Paris"}`
	b := baseFailure()
	b.ArgsJSON = `{"location":"Austin"}`

	assert.Equal(t, Signature(a), Signature(b), "argument VALUES are per-call noise")
}

// TestSignatureCollapsesQuotedArgumentNames documents an interaction that is
// easy to misread as a bug: because step 3 of the normalization replaces every
// quoted run, `argument "location" must be ...` and `argument "units" must be
// ...` normalize to the same shape. They stay DIFFERENT failures anyway, because
// the argument key set is a separate component of the signature — which is the
// reason that component exists.
func TestSignatureCollapsesQuotedArgumentNames(t *testing.T) {
	a := baseFailure()
	a.ErrorMessage = `argument "location" must be at least 2 characters`
	a.ArgsJSON = `{"location":"x"}`

	sameKeys := a
	sameKeys.ErrorMessage = `argument "units" must be at least 2 characters`
	assert.Equal(t, Signature(a), Signature(sameKeys),
		"the message shape is the defect; the quoted name alone is not")

	realWorld := sameKeys
	realWorld.ArgsJSON = `{"units":"c"}`
	assert.NotEqual(t, Signature(a), Signature(realWorld),
		"in practice the offending argument is also in the arg key set")
}

func TestSignatureDistinguishesToolAndCode(t *testing.T) {
	base := baseFailure()

	otherTool := base
	otherTool.Tool = "web_lookup"
	assert.NotEqual(t, Signature(base), Signature(otherTool))

	otherCode := base
	otherCode.ErrorCode = tools.CodeUpstreamError
	assert.NotEqual(t, Signature(base), Signature(otherCode))
}

func TestSignatureShape(t *testing.T) {
	sig := Signature(baseFailure())
	assert.Len(t, sig, signatureHexLen)
	assert.Equal(t, strings.ToLower(sig), sig)
	assert.NotContains(t, sig, "#")
}

func TestNormalizeErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"lowercases", "Argument Location", "argument location"},
		{"collapses digit runs", "retry after 4500 ms", "retry after # ms"},
		{"collapses quoted runs", `argument "location" bad`, `argument "?" bad`},
		{"collapses whitespace", "a\n\t  b", "a b"},
		{"trims", "  padded  ", "padded"},
		{
			"the headline invalid_args shape",
			`argument "seconds" must be <= 86400`,
			`argument "?" must be <= #`,
		},
		{
			"two different numbers collapse to one shape",
			`argument "seconds" must be <= 3600`,
			`argument "?" must be <= #`,
		},
		{"empty stays empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NormalizeErrorMessage(tc.in))
		})
	}

	// Step 6: rune truncation at 200.
	long := NormalizeErrorMessage(strings.Repeat("é", 500))
	assert.Equal(t, 200, len([]rune(long)))
}

func TestFamilySanitizesKeyComponents(t *testing.T) {
	tests := []struct {
		name, tool, code, want string
	}{
		{"plain", "get_weather", "invalid_args", "RCA#get_weather#invalid_args"},
		{"hash injection", "a#b", "invalid_args", "RCA#a_b#invalid_args"},
		{"uppercase", "UPPER", "CODE", "RCA#upper#code"},
		{"empty", "", "", "RCA#none#none"},
		{"newline", "tool\nname", "invalid_args", "RCA#tool_name#invalid_args"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Family(tc.tool, tc.code)
			assert.Equal(t, tc.want, got)
			// Exactly the two structural '#' separators, never a third.
			assert.Equal(t, 2, strings.Count(got, "#"))
		})
	}

	// A 200-rune client-supplied name cannot blow the key out.
	long := Family(strings.Repeat("z", 200), "invalid_args")
	assert.LessOrEqual(t, len(long), len("RCA#")+maxKeyComponentRunes+1+maxKeyComponentRunes)
	assert.Equal(t, 2, strings.Count(long, "#"))
}

func TestArgKeys(t *testing.T) {
	assert.Equal(t, []string{"location", "units"}, ArgKeys(`{"units":"metric","location":"x"}`))
	assert.Nil(t, ArgKeys(`{}`))
	assert.Nil(t, ArgKeys(``))
	assert.Nil(t, ArgKeys(`not json`))
	assert.Nil(t, ArgKeys(`["a","b"]`), "a non-object yields nil, never an error")
	// A producer-truncated blob must still be signable.
	assert.Nil(t, ArgKeys(`{"location":"aaaa`))
}

// TestDayKeyIsUTC pins the cap's bucket boundary: the counter is an AWS-account
// cost control, so it rolls at UTC midnight regardless of anyone's timezone.
func TestDayKeyIsUTC(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable on this machine")
	}
	local := time.Date(2026, 7, 25, 23, 30, 0, 0, ny) // 03:30Z on the 26th
	assert.Equal(t, "2026-07-26", DayKey(local))
	assert.Equal(t, "2026-07-25", DayKey(fixedTestNow))
}

func TestCapReachedBoundary(t *testing.T) {
	assert.False(t, CapReached(9, 10))
	assert.True(t, CapReached(10, 10))
	assert.True(t, CapReached(11, 10))
}

func TestNewIDShape(t *testing.T) {
	id, err := NewID()
	require.NoError(t, err)
	assert.Len(t, id, 12)
	assert.Equal(t, strings.ToLower(id), id)
	assert.NotContains(t, id, "#")
}
