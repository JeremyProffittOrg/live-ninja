package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func newTestStore() (*Store, *testutil.FakeDynamo) {
	fake := testutil.NewFakeDynamo()
	return NewWithClient(fake, "live-ninja-test"), fake
}

func mkSession(id, userID, familyID, refreshHash string) *Session {
	return &Session{
		SessionID:   id,
		UserID:      userID,
		FamilyID:    familyID,
		Surface:     SurfaceWeb,
		RefreshHash: refreshHash,
		ExpiresAt:   time.Now().Add(24 * time.Hour).Unix(),
		TTL:         time.Now().Add(24 * time.Hour).Unix(),
	}
}

func TestCreateAndGetSession(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	sess := mkSession("s1", "u1", "f1", "hash-1")
	require.NoError(t, st.CreateSession(ctx, sess))

	got, err := st.GetSessionByID(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "u1", got.UserID)
	assert.Equal(t, "hash-1", got.RefreshHash)

	// Session id collision is rejected, not upserted.
	err = st.CreateSession(ctx, mkSession("s1", "u1", "f1", "hash-x"))
	require.ErrorIs(t, err, ErrAlreadyExists)

	// Unknown id -> (nil, nil).
	missing, err := st.GetSessionByID(ctx, "nope")
	require.NoError(t, err)
	assert.Nil(t, missing)
}

func TestRotateRefreshHappyPath(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	sess := mkSession("s1", "u1", "f1", "hash-old")
	require.NoError(t, st.CreateSession(ctx, sess))

	slideTo := time.Now().Add(30 * 24 * time.Hour).Unix()
	rotated, err := st.RotateRefresh(ctx, sess, "hash-old", "hash-new", slideTo)
	require.NoError(t, err)
	assert.Equal(t, "hash-new", rotated.RefreshHash)
	assert.Equal(t, "hash-old", rotated.PrevHash)
	assert.Equal(t, slideTo, rotated.ExpiresAt)
	assert.Equal(t, slideTo, rotated.TTL)

	// Persisted state matches.
	fresh, err := st.GetSessionByID(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, "hash-new", fresh.RefreshHash)
	assert.Equal(t, "hash-old", fresh.PrevHash)
	assert.Equal(t, slideTo, fresh.ExpiresAt)
}

func TestRotateRefreshReuseDetectionRevokesFamily(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	// Two sessions in the same family plus one in another family.
	require.NoError(t, st.CreateSession(ctx, mkSession("s1", "u1", "fam-A", "hash-1")))
	require.NoError(t, st.CreateSession(ctx, mkSession("s2", "u1", "fam-A", "hash-2")))
	require.NoError(t, st.CreateSession(ctx, mkSession("s3", "u1", "fam-B", "hash-3")))

	// Legitimate rotate on s1: hash-1 becomes prevHash.
	sess, err := st.GetSessionByID(ctx, "s1")
	require.NoError(t, err)
	rotated, err := st.RotateRefresh(ctx, sess, "hash-1", "hash-1b", time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)

	// Replay of the ALREADY-ROTATED token -> reuse detected, whole
	// fam-A revoked; fam-B untouched.
	_, err = st.RotateRefresh(ctx, rotated, "hash-1", "hash-evil", time.Now().Add(time.Hour).Unix())
	require.ErrorIs(t, err, ErrRefreshReuse)

	s1, err := st.GetSessionByID(ctx, "s1")
	require.NoError(t, err)
	assert.Nil(t, s1, "s1 must be revoked")
	s2, err := st.GetSessionByID(ctx, "s2")
	require.NoError(t, err)
	assert.Nil(t, s2, "sibling s2 in the same family must be revoked")
	s3, err := st.GetSessionByID(ctx, "s3")
	require.NoError(t, err)
	assert.NotNil(t, s3, "fam-B session must survive")
}

func TestRotateRefreshStaleCopyLosesRaceToReuseDetection(t *testing.T) {
	// Simulates the double-spend race: a caller holds a session copy whose
	// RefreshHash still equals the presented hash, but the row has already
	// been rotated (presented == stored prevHash). The transaction's
	// condition fails, the consistent re-read adjudicates it as reuse, and
	// the family is revoked.
	ctx := context.Background()
	st, _ := newTestStore()

	require.NoError(t, st.CreateSession(ctx, mkSession("s1", "u1", "fam-A", "hash-1")))
	stale, err := st.GetSessionByID(ctx, "s1")
	require.NoError(t, err)

	// The "other request" rotates first.
	_, err = st.RotateRefresh(ctx, stale, "hash-1", "hash-2", time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)

	// Our stale copy still says RefreshHash == hash-1, so RotateRefresh
	// goes down the transaction path and loses the condition.
	_, err = st.RotateRefresh(ctx, stale, "hash-1", "hash-3", time.Now().Add(time.Hour).Unix())
	require.ErrorIs(t, err, ErrRefreshReuse)

	gone, err := st.GetSessionByID(ctx, "s1")
	require.NoError(t, err)
	assert.Nil(t, gone, "family must be revoked after adjudicated reuse")
}

func TestRotateRefreshInvalidHash(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	require.NoError(t, st.CreateSession(ctx, mkSession("s1", "u1", "f1", "hash-1")))
	sess, err := st.GetSessionByID(ctx, "s1")
	require.NoError(t, err)

	_, err = st.RotateRefresh(ctx, sess, "hash-garbage", "hash-new", time.Now().Add(time.Hour).Unix())
	require.ErrorIs(t, err, ErrInvalidRefresh)

	// The session survives an invalid presentation (no revoke).
	still, err := st.GetSessionByID(ctx, "s1")
	require.NoError(t, err)
	assert.NotNil(t, still)

	_, err = st.RotateRefresh(ctx, nil, "h", "h2", 1)
	require.ErrorIs(t, err, ErrInvalidRefresh)
	_, err = st.RotateRefresh(ctx, sess, "", "h2", 1)
	require.ErrorIs(t, err, ErrInvalidRefresh)
}

func TestRevokeAllForUserAndListSessions(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	a := mkSession("s1", "u1", "f1", "h1")
	a.LastUsedAt = time.Now().Add(-time.Hour).Unix()
	b := mkSession("s2", "u1", "f2", "h2")
	b.LastUsedAt = time.Now().Unix()
	other := mkSession("s9", "u2", "f9", "h9")
	require.NoError(t, st.CreateSession(ctx, a))
	require.NoError(t, st.CreateSession(ctx, b))
	require.NoError(t, st.CreateSession(ctx, other))

	list, err := st.ListSessions(ctx, "u1")
	require.NoError(t, err)
	require.Len(t, list, 2)
	// Most-recently-used first.
	assert.Equal(t, "s2", list[0].SessionID)
	assert.Equal(t, "s1", list[1].SessionID)

	require.NoError(t, st.RevokeAllForUser(ctx, "u1"))
	list, err = st.ListSessions(ctx, "u1")
	require.NoError(t, err)
	assert.Empty(t, list)

	// The other user's session is untouched.
	s9, err := st.GetSessionByID(ctx, "s9")
	require.NoError(t, err)
	assert.NotNil(t, s9)
}

func TestRevokeSessionIdempotent(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()
	require.NoError(t, st.CreateSession(ctx, mkSession("s1", "u1", "f1", "h1")))
	require.NoError(t, st.RevokeSession(ctx, "u1", "s1"))
	require.NoError(t, st.RevokeSession(ctx, "u1", "s1")) // no-op, no error
}
