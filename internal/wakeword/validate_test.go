package wakeword

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidatePhraseAcceptsAndNormalizes(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hey ninja", "hey ninja"},
		{"  Hey   Ninja  ", "hey ninja"},
		{"OK Computer Please", "ok computer please"},
		{"one two three four", "one two three four"},
		{"HEY\tLIVE\nNINJA", "hey live ninja"},
	}
	for _, tc := range cases {
		got, msg := ValidatePhrase(tc.in)
		require.Empty(t, msg, "phrase %q should validate", tc.in)
		assert.Equal(t, tc.want, got)
	}
}

func TestValidatePhraseRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"one word", "ninja"},
		{"five words", "one two three four five"},
		{"digits", "hey ninja2"},
		{"punctuation", "hey ninja!"},
		{"apostrophe", "hey ninja's"},
		{"non-latin", "héy ninja"},
		{"cyrillic", "привет ниндзя"},
		{"single-letter word", "a ninja"},
		{"word too long", "hey supercalifragilistic"},
		{"profanity", "hey shit"},
		{"profanity uppercase", "Hey SHIT"},
		{"total too long", "abcdefghijklmn abcdefghijklmn abcdefghijklmn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := ValidatePhrase(tc.in)
			assert.Empty(t, got)
			assert.NotEmpty(t, msg, "phrase %q must be rejected", tc.in)
		})
	}
}

func TestSlug(t *testing.T) {
	assert.Equal(t, "hey-live-ninja", Slug("hey live ninja"))
}

func TestWakewordIDDeterministicAndUserSalted(t *testing.T) {
	a1 := WakewordID("user-a", "hey ninja")
	a2 := WakewordID("user-a", "hey ninja")
	b := WakewordID("user-b", "hey ninja")

	// Deterministic per (user, phrase): retraining reuses the same id/prefix.
	assert.Equal(t, a1, a2)
	// User-salted: two users never share an S3 prefix for the same phrase.
	assert.NotEqual(t, a1, b)

	// Shape: slug + "-" + 6 hex chars.
	require.True(t, strings.HasPrefix(a1, "hey-ninja-"), a1)
	suffix := strings.TrimPrefix(a1, "hey-ninja-")
	assert.Len(t, suffix, 6)
	assert.Equal(t, strings.ToLower(suffix), suffix)
}

func TestCollidesWithBuiltin(t *testing.T) {
	// By phrase and by id-slug ("hey-jarvis" is the client-bundled builtin).
	assert.True(t, collidesWithBuiltin("hey jarvis"))
	assert.True(t, collidesWithBuiltin("hi esp"))
	// "hey live ninja" is trainable — no client ships a model for it.
	assert.False(t, collidesWithBuiltin("hey live ninja"))
	assert.False(t, collidesWithBuiltin("hey purple parrot"))
}
