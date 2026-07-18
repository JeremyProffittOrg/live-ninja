package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordAndListConsents(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	// Empty ledger.
	none, err := st.ListConsents(ctx, "u1")
	require.NoError(t, err)
	assert.Empty(t, none)

	first, err := st.RecordConsent(ctx, "u1", "web", "2026-07-privacy-v1", "2026-07-17T12:00:00Z")
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, "web", first.Surface)
	assert.Equal(t, "2026-07-privacy-v1", first.Version)
	assert.Equal(t, "2026-07-17T12:00:00Z", first.ClientTS)
	_, err = time.Parse(time.RFC3339Nano, first.TS)
	assert.NoError(t, err, "server ts must be RFC3339Nano")

	// A second event (different surface, no client ts) appends — never
	// overwrites. The sleep guarantees a later server timestamp even on
	// coarse-clock hosts, keeping the order assertion below meaningful.
	time.Sleep(2 * time.Millisecond)
	second, err := st.RecordConsent(ctx, "u1", "android", "2026-07-privacy-v2", "")
	require.NoError(t, err)
	assert.Empty(t, second.ClientTS)

	got, err := st.ListConsents(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	// Oldest-first by sort key (server timestamps ascend).
	assert.Equal(t, "2026-07-privacy-v1", got[0].Version)
	assert.Equal(t, "2026-07-privacy-v2", got[1].Version)

	// Another user's ledger is untouched.
	other, err := st.ListConsents(ctx, "u2")
	require.NoError(t, err)
	assert.Empty(t, other)
}

func TestRecordConsentValidation(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	_, err := st.RecordConsent(ctx, "", "web", "v1", "")
	assert.Error(t, err)
	_, err = st.RecordConsent(ctx, "u1", "", "v1", "")
	assert.Error(t, err)
	_, err = st.RecordConsent(ctx, "u1", "web", " ", "")
	assert.Error(t, err)
}
