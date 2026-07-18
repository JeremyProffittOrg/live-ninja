package store

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mkDeliverable(userID, id, createdAt, name string) *Deliverable {
	return &Deliverable{
		DeliverableID: id,
		UserID:        userID,
		Name:          name,
		ContentType:   "text/markdown; charset=utf-8",
		Kind:          DeliverableKindFile,
		Status:        DeliverableStatusReady,
		S3Key:         "deliverables/" + userID + "/" + id + "/" + name,
		SizeBytes:     42,
		CreatedAt:     createdAt,
	}
}

func TestCreateAndGetDeliverable(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	d := mkDeliverable("u1", "d1", "2026-07-17T10:00:00Z", "report.md")
	require.NoError(t, st.CreateDeliverable(ctx, d))

	got, err := st.GetDeliverable(ctx, "u1", "d1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "report.md", got.Name)
	assert.Equal(t, DeliverableStatusReady, got.Status)
	assert.Equal(t, "deliverables/u1/d1/report.md", got.S3Key)
	assert.Equal(t, int64(42), got.SizeBytes)
	assert.Equal(t, "DELIV#2026-07-17T10:00:00Z#d1", got.SK())

	// Same createdAt+id again is a conditional-put conflict.
	require.ErrorIs(t, st.CreateDeliverable(ctx, d), ErrAlreadyExists)
}

func TestGetDeliverableOwnershipAndAbsence(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	require.NoError(t, st.CreateDeliverable(ctx,
		mkDeliverable("u1", "d1", "2026-07-17T10:00:00Z", "a.md")))

	// Another user's id must be indistinguishable from an absent one.
	got, err := st.GetDeliverable(ctx, "u2", "d1")
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = st.GetDeliverable(ctx, "u2", "nope")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestListDeliverablesPaginatesNewestFirst(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	base := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		d := mkDeliverable("u1", fmt.Sprintf("d%d", i),
			base.Add(time.Duration(i)*time.Minute).Format(time.RFC3339),
			fmt.Sprintf("f%d.md", i))
		require.NoError(t, st.CreateDeliverable(ctx, d))
	}
	// Another user's rows must never bleed into u1's listing.
	require.NoError(t, st.CreateDeliverable(ctx,
		mkDeliverable("u2", "other", base.Format(time.RFC3339), "theirs.md")))

	page1, cur, err := st.ListDeliverables(ctx, "u1", 2, "")
	require.NoError(t, err)
	require.Len(t, page1, 2)
	assert.Equal(t, "d4", page1[0].DeliverableID) // newest first
	assert.Equal(t, "d3", page1[1].DeliverableID)
	require.NotEmpty(t, cur)

	page2, cur, err := st.ListDeliverables(ctx, "u1", 2, cur)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	assert.Equal(t, "d2", page2[0].DeliverableID)
	assert.Equal(t, "d1", page2[1].DeliverableID)
	require.NotEmpty(t, cur)

	page3, cur, err := st.ListDeliverables(ctx, "u1", 2, cur)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	assert.Equal(t, "d0", page3[0].DeliverableID)
	assert.Empty(t, cur, "final page must not return a cursor")
}

func TestListDeliverablesRejectsBadCursor(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	_, _, err := st.ListDeliverables(ctx, "u1", 10, "!!!not-base64!!!")
	require.Error(t, err)

	// Well-formed base64 that decodes to a non-DELIV# sk must be rejected
	// (a tampered cursor can never address outside the DELIV# range).
	evil := base64.RawURLEncoding.EncodeToString([]byte("SETTINGS"))
	_, _, err = st.ListDeliverables(ctx, "u1", 10, evil)
	require.Error(t, err)
}

func TestUpdateDeliverableStatus(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore()

	d := mkDeliverable("u1", "z1", "2026-07-17T10:00:00Z", "bundle.zip")
	d.Kind = DeliverableKindZip
	d.Status = DeliverableStatusPending
	d.SizeBytes = 0
	require.NoError(t, st.CreateDeliverable(ctx, d))

	require.NoError(t, st.UpdateDeliverableStatus(ctx, "u1", d.SK(), DeliverableStatusReady, 1234))

	got, err := st.GetDeliverable(ctx, "u1", "z1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, DeliverableStatusReady, got.Status)
	assert.Equal(t, int64(1234), got.SizeBytes)

	// Write-back against a deleted/absent item is ErrNotFound, not an upsert.
	err = st.UpdateDeliverableStatus(ctx, "u1", deliverableSK("2026-01-01T00:00:00Z", "ghost"),
		DeliverableStatusFailed, 0)
	require.ErrorIs(t, err, ErrNotFound)

	// A non-DELIV sk is refused before any write.
	require.Error(t, st.UpdateDeliverableStatus(ctx, "u1", "SETTINGS", DeliverableStatusReady, 1))
}

func TestDeleteDeliverable(t *testing.T) {
	ctx := context.Background()
	st, fake := newTestStore()

	d := mkDeliverable("u1", "d1", "2026-07-17T10:00:00Z", "a.md")
	require.NoError(t, st.CreateDeliverable(ctx, d))
	require.Equal(t, 1, fake.Len())

	require.NoError(t, st.DeleteDeliverable(ctx, "u1", d.SK()))
	require.Equal(t, 0, fake.Len())

	// Deleting an absent key is a no-op; a non-DELIV sk is refused.
	require.NoError(t, st.DeleteDeliverable(ctx, "u1", d.SK()))
	require.Error(t, st.DeleteDeliverable(ctx, "u1", "PROFILE"))
}
