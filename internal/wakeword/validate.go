// Package wakeword implements the M6 programmable wake-word backend
// (plan.md M6 training-pipeline + distribution tasks, FR-K01..06):
// phrase validation, the AWS Batch openWakeWord training submission
// (≤3/day/user, bounded queue), lazy training-status finalization from
// S3 manifests + Batch DescribeJobs, the per-user catalog (builtins +
// custom models, 5-min in-memory cache), and the per-platform model
// manifest (presigned 15-min URL + sha256) per
// contracts/wakeword-manifest.md.
//
// Locked autonomous decisions carried in this package (2026-07-17):
//   - openWakeWord is the ONLY server-side training path. Porcupine
//     needs a Picovoice account (external dependency) — the engine is
//     surfaced in the catalog as not-trainable, never faked.
//   - Custom models are produced as int8 .onnx for BOTH web and android
//     (deviation from wakeword-manifest.md's oww-tflite-android-v1
//     sketch; the android format tag "oww-onnx-android-v1" is a new
//     additive (engine, platform, format) combination, allowed by
//     contracts/README.md rule 1/3 — android's ModelManager verifies
//     sha256 over whatever asset the manifest names).
//   - esp32 gets curated builtin WakeNet models only; custom-on-esp32
//     is honestly reported unsupported (no oWW-ESP conversion yet).
package wakeword

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Phrase rules (plan.md M6 "validate (phoneme/profanity/collision)"):
// 2–4 words, latin letters only. Per-word and total length bounds keep
// the phrase pronounceable and the derived slug/S3 key sane.
const (
	minPhraseWords = 2
	maxPhraseWords = 4
	minWordLen     = 2
	maxWordLen     = 14
	maxPhraseLen   = 40 // normalized, spaces included
)

// profanity is a deliberately small embedded denylist checked against
// each normalized word. This is a single-owner + allowlist product
// (plan.md locked decisions) — the list guards against accidental
// embarrassment (the trained phrase lands in SES mail and device UIs),
// not against a determined adversary.
var profanity = map[string]struct{}{
	"anal": {}, "anus": {}, "arse": {}, "ass": {}, "asshole": {},
	"bastard": {}, "bitch": {}, "bollocks": {}, "boob": {}, "boobs": {},
	"bugger": {}, "clit": {}, "cock": {}, "crap": {}, "cum": {},
	"cunt": {}, "dick": {}, "dildo": {}, "douche": {}, "fag": {},
	"faggot": {}, "fuck": {}, "fucker": {}, "fucking": {}, "jerk": {},
	"jizz": {}, "kike": {}, "milf": {}, "negro": {}, "nigga": {},
	"nigger": {}, "penis": {}, "piss": {}, "porn": {}, "prick": {},
	"pussy": {}, "rape": {}, "retard": {}, "scrotum": {}, "sex": {},
	"shit": {}, "slut": {}, "spic": {}, "tit": {}, "tits": {},
	"twat": {}, "vagina": {}, "wank": {}, "wanker": {}, "whore": {},
}

// ValidatePhrase normalizes and validates a candidate wake phrase.
// Returns the normalized phrase (lowercase, single-space separated) and
// "" on success, or ("", msg) with a human-readable, field-adjacent
// error message (surfaced verbatim by the HTTP 400).
func ValidatePhrase(phrase string) (string, string) {
	words := strings.Fields(strings.ToLower(strings.TrimSpace(phrase)))
	if len(words) < minPhraseWords || len(words) > maxPhraseWords {
		return "", "wake phrase must be 2 to 4 words (e.g. \"hey ninja\")"
	}
	for _, w := range words {
		if len(w) < minWordLen || len(w) > maxWordLen {
			return "", "each word must be 2 to 14 letters"
		}
		for _, r := range w {
			if r < 'a' || r > 'z' {
				return "", "wake phrase may contain latin letters and spaces only (no digits or punctuation)"
			}
		}
		if _, bad := profanity[w]; bad {
			return "", "that phrase isn't allowed — pick different words"
		}
	}
	normalized := strings.Join(words, " ")
	if len(normalized) > maxPhraseLen {
		return "", "wake phrase must be at most 40 characters"
	}
	return normalized, ""
}

// Slug converts a normalized phrase into its id-safe form
// ("hey live ninja" → "hey-live-ninja").
func Slug(normalized string) string {
	return strings.ReplaceAll(normalized, " ", "-")
}

// WakewordID derives the deterministic wwId for a (user, phrase) pair:
// the phrase slug plus a 6-hex-char user-salted suffix. Deterministic so
// the same user retraining the same phrase reuses one id (and one S3
// prefix), user-salted so two users training the same phrase never
// share an S3 prefix (S3 keys are wakewords/<wwId>/... — not
// user-partitioned — while the DynamoDB item IS user-partitioned).
func WakewordID(userID, normalized string) string {
	slug := Slug(normalized)
	sum := sha256.Sum256([]byte(userID + "|" + slug))
	return slug + "-" + hex.EncodeToString(sum[:3])
}
