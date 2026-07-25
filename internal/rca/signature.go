package rca

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/JeremyProffittOrg/live-ninja/internal/tools"
)

const (
	// familyPrefix is the leading-key namespace every RCA partition shares
	// (records, cooldown markers, the budget counter and the notice markers),
	// so ONE IAM dynamodb:LeadingKeys condition covers the analyzer's whole
	// non-user write surface.
	familyPrefix = "RCA#"

	// maxKeyComponentRunes caps each sanitized key component. DynamoDB's own
	// key limit is far higher; this exists so a pathological client-supplied
	// tool name cannot produce an unwieldy partition key.
	maxKeyComponentRunes = 64

	// signatureHexLen is how much of the SHA-256 digest a signature keeps.
	// 16 hex chars = 64 bits, which is overwhelming collision headroom for a
	// key space of "distinct failure shapes seen in 30 days" (single digits).
	signatureHexLen = 16
)

// Family is the DynamoDB partition every RCA record, cooldown marker and
// suppression counter for one tool+errorCode pair lives in:
// "RCA#<tool>#<errorCode>".
//
// Both components are sanitized unconditionally. ErrorCode is a closed set,
// but Tool is client-supplied on the unknown_tool path — tools.Invoke returns
// CodeUnknownTool with the raw requested name before any validation runs — so
// an unsanitized component would give unbounded partition fan-out plus '#'
// injection into the key. (The producer additionally replaces that name with
// the "unknown_tool" sentinel; this is the second, independent guard.)
func Family(tool, errorCode string) string {
	return familyPrefix + sanitizeKeyComponent(tool) + "#" + sanitizeKeyComponent(errorCode)
}

// sanitizeKeyComponent lowercases, keeps only [a-z0-9_], maps everything else
// (including '#', whitespace and control characters) to '_', and truncates to
// maxKeyComponentRunes. Empty input yields "none" so a key component is never
// blank.
func sanitizeKeyComponent(s string) string {
	if s == "" {
		return "none"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= maxKeyComponentRunes {
			break
		}
	}
	out := b.String()
	if out == "" {
		return "none"
	}
	return out
}

// Signature is the dedupe identity of a failure: two failures with the same
// signature are "the same failure", and the second is suppressed by the
// cooldown. It is SHA-256 over
//
//	tool "\n" errorCode "\n" NormalizeErrorMessage(msg) "\n" strings.Join(argKeys, ",")
//
// rendered as the first 16 lowercase hex characters.
//
// argKeys is the SORTED set of top-level keys in ArgsJSON — keys, never
// values. Rationale, per key component:
//   - tool + errorCode: obviously identity-bearing.
//   - normalized message: invalid_args messages name the offending argument
//     (`argument "seconds" must be <= 86400`), which is the actual defect; the
//     numbers and quoted values in them are per-call noise.
//   - arg key set: "get_weather called with {location}" and "get_weather
//     called with {location,units}" are different prompt/schema bugs even when
//     the message shape collapses to the same text.
//
// Deliberately EXCLUDED: userId, sessionId, callId, txId, occurredAt, and
// argument values. Cross-session recurrence of the same defect is precisely
// the signal RCA exists to catch, so including any per-call identifier would
// make every failure unique and the cooldown a no-op.
//
// NOTE (corrects archive/base-knowledge-plan.md §M17): the archived design put
// a rootCauseHash in this key. rootCause is Opus OUTPUT, so a signature
// containing it could only be computed after paying for the analysis — the
// cooldown must gate the model call, not follow it.
func Signature(f tools.ToolFailure) string {
	h := sha256.New()
	h.Write([]byte(f.Tool))
	h.Write([]byte{'\n'})
	h.Write([]byte(f.ErrorCode))
	h.Write([]byte{'\n'})
	h.Write([]byte(NormalizeErrorMessage(f.ErrorMessage)))
	h.Write([]byte{'\n'})
	h.Write([]byte(strings.Join(ArgKeys(f.ArgsJSON), ",")))
	return hex.EncodeToString(h.Sum(nil))[:signatureHexLen]
}

// maxNormalizedMessageRunes caps a normalized message so a pathological
// upstream error body cannot dominate the hash input (or, via the same
// helper, a log line).
const maxNormalizedMessageRunes = 200

var (
	digitRunRe  = regexp.MustCompile(`[0-9]+`)
	quotedRunRe = regexp.MustCompile(`"[^"]*"`)
)

// NormalizeErrorMessage collapses a ToolError message to its shape, in this
// exact order (each step is asserted individually by TestNormalizeErrorMessage):
//  1. strings.ToLower
//  2. every run of one or more ASCII digits -> "#"
//  3. every double-quoted run -> `"?"`
//  4. every run of unicode whitespace -> a single space
//  5. strings.TrimSpace
//  6. rune-truncate to 200
//
// The order matters: digits are collapsed before quoted runs so that a quoted
// numeric value is not first turned into `"#"` and then into `"?"` by two
// different rules — the outcome is the same, but only one rule ever "owns" a
// given span, which is what makes the table-driven test per-step assertable.
func NormalizeErrorMessage(s string) string {
	s = strings.ToLower(s)
	s = digitRunRe.ReplaceAllString(s, "#")
	s = quotedRunRe.ReplaceAllString(s, `"?"`)
	s = collapseWhitespace(s)
	s = strings.TrimSpace(s)
	return truncateRunes(s, maxNormalizedMessageRunes)
}

// collapseWhitespace replaces every run of unicode whitespace with one space.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inRun := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !inRun {
				b.WriteRune(' ')
				inRun = true
			}
			continue
		}
		inRun = false
		b.WriteRune(r)
	}
	return b.String()
}

// truncateRunes rune-truncates s to at most n runes (never mid-rune).
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ArgKeys returns the sorted top-level keys of a JSON object string. A
// non-object or unparseable input yields nil (never an error): the signature
// must be computable for any input, including the truncated ArgsJSON the
// producer's 2 KB cap can leave behind.
func ArgKeys(argsJSON string) []string {
	if strings.TrimSpace(argsJSON) == "" {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &obj); err != nil {
		return nil
	}
	if len(obj) == 0 {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// DayKey is the UTC calendar day the daily cap is bucketed by, "2006-01-02".
// UTC, not the profile timezone: the cap is a cost control on the AWS account,
// not a user-facing window, and a per-user timezone would make the counter
// non-atomic across users.
func DayKey(now time.Time) string {
	return now.UTC().Format("2006-01-02")
}

// CapReached reports whether an observed count has consumed the day's budget.
// Pure and trivial, but named and tested because the off-by-one (>= vs >) is
// the whole cap.
func CapReached(count, cap int) bool { return count >= cap }
