package codeapproval

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/ghost"
	"github.com/stretchr/testify/require"
)

type testCatalog struct{ err error }

func (c testCatalog) ListRepos(context.Context) ([]ghost.Repo, error) {
	return []ghost.Repo{{Repo: "org/app", Owner: "org", Name: "app"}}, c.err
}
func (c testCatalog) Nodes(context.Context, string) ([]ghost.Node, error) {
	return []ghost.Node{{NodeID: "OFFICEPC", Status: "ONLINE"}}, c.err
}
func testInput() Input {
	return Input{Repo: "org/app", Node: "OFFICEPC", Instructions: "Improve the retry behavior", Agent: "codex"}
}
func testService(t *testing.T) (*Service, *FileStore) {
	t.Helper()
	st, e := NewFileStore(filepath.Join(t.TempDir(), "approvals.json"))
	require.NoError(t, e)
	t.Cleanup(func() { _ = st.Close() })
	return NewService(st, testCatalog{}), st
}
func TestPrepareVerifiesImmutableExactActionAndDeduplicates(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	in := testInput()
	in.Instructions = "  Improve the retry behavior\nKeep the next line.  "
	in.Model = " proposed-model "
	in.Effort = " high "
	i, e := s.Prepare(ctx, "owner", in, "prepare-0001")
	require.NoError(t, e)
	require.Equal(t, StatusPrepared, i.Status)
	require.Equal(t, int64(1), i.Version)
	require.True(t, i.Verification.RepoVerified)
	require.True(t, i.Verification.NodeVerified)
	require.False(t, i.Verification.LaunchPermissionVerified)
	require.False(t, i.Action.Deploy)
	require.Equal(t, "Improve the retry behavior\nKeep the next line.", i.Action.Instructions)
	require.Equal(t, "proposed-model", i.Action.Model)
	replay, e := s.Prepare(ctx, "owner", in, "prepare-0001")
	require.NoError(t, e)
	require.Equal(t, i, replay)
	in.Instructions = "A different requested change"
	_, e = s.Prepare(ctx, "owner", in, "prepare-0001")
	require.ErrorIs(t, e, ErrConflict)
	// The returned object cannot mutate the stored review.
	i.Action.Instructions = "mutated client object"
	stored, e := s.Get(ctx, "owner", i.ID)
	require.NoError(t, e)
	require.NotEqual(t, i.Action.Instructions, stored.Action.Instructions)
	_, e = s.Get(ctx, "another-owner", i.ID)
	require.ErrorIs(t, e, ErrNotFound)
}
func TestPrepareRejectsUnavailableOrUnknownFleetAndDeploy(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	for _, change := range []func(*Input){func(i *Input) { i.Repo = "org/missing" }, func(i *Input) { i.Node = "MISSING" }, func(i *Input) { i.Deploy = true }, func(i *Input) { i.Agent = "arbitrary-executable" }} {
		in := testInput()
		change(&in)
		_, e := s.Prepare(ctx, "owner", in, "prepare-invalid")
		require.ErrorIs(t, e, ErrValidation)
	}
	s.catalog = testCatalog{err: errors.New("offline")}
	_, e := s.Prepare(ctx, "owner", testInput(), "prepare-offline")
	require.ErrorIs(t, e, ErrUnavailable)
	var missing *ghost.Client
	s.catalog = missing
	_, e = s.Options(ctx)
	require.ErrorIs(t, e, ErrUnavailable)
	page, e := s.List(ctx, "owner", 30, "")
	require.NoError(t, e)
	require.Empty(t, page.Intents)
}
func TestApproveIsSingleUseBoundToVersionAndReturnsDurableBlockedReceipt(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	i, e := s.Prepare(ctx, "owner", testInput(), "prepare-0001")
	require.NoError(t, e)
	_, e = s.Approve(ctx, "another-owner", i.ID, i.Version, i.ActionHash, "approve-other")
	require.ErrorIs(t, e, ErrNotFound)
	_, e = s.Approve(ctx, "owner", i.ID, i.Version+1, i.ActionHash, "approve-wrong")
	require.ErrorIs(t, e, ErrConflict)
	approved, e := s.Approve(ctx, "owner", i.ID, i.Version, i.ActionHash, "approve-0001")
	require.NoError(t, e)
	require.Equal(t, StatusApprovedBlocked, approved.Status)
	require.Equal(t, int64(2), approved.Version)
	require.Equal(t, i.Action, approved.Action)
	require.Equal(t, i.ActionHash, approved.Receipt.ActionHash)
	require.Equal(t, "not_started", approved.Receipt.ExecutionState)
	require.True(t, approved.Receipt.RequiresFreshApproval)
	require.Equal(t, BlockedReason, approved.Receipt.Reason)
	replay, e := s.Approve(ctx, "owner", i.ID, i.Version, i.ActionHash, "approve-0001")
	require.NoError(t, e)
	require.Equal(t, approved, replay)
	_, e = s.Approve(ctx, "owner", i.ID, i.Version, i.ActionHash, "approve-again")
	require.ErrorIs(t, e, ErrConflict)
	// Restart/recovery reads the same receipt; nothing can dispatch it later.
	path := st.path
	require.NoError(t, st.Close())
	reopened, e := NewFileStore(path)
	require.NoError(t, e)
	defer reopened.Close()
	recovered := NewService(reopened, testCatalog{})
	got, e := recovered.Get(ctx, "owner", i.ID)
	require.NoError(t, e)
	require.Equal(t, approved, got)
	require.False(t, recovered.Capabilities().Execute)
}
func TestApprovalExpiryAndRetainedReceiptReplay(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	expired, e := s.Prepare(ctx, "owner", testInput(), "prepare-expired")
	require.NoError(t, e)
	active, e := s.Prepare(ctx, "owner", testInput(), "prepare-approved")
	require.NoError(t, e)
	approved, e := s.Approve(ctx, "owner", active.ID, active.Version, active.ActionHash, "approve-on-time")
	require.NoError(t, e)
	now = now.Add(IntentTTL)
	_, e = s.Approve(ctx, "owner", expired.ID, expired.Version, expired.ActionHash, "approve-too-late")
	require.ErrorIs(t, e, ErrExpired)
	got, e := s.Get(ctx, "owner", expired.ID)
	require.NoError(t, e)
	require.Equal(t, "expired", got.Status)
	replay, e := s.Approve(ctx, "owner", active.ID, active.Version, active.ActionHash, "approve-on-time")
	require.NoError(t, e)
	require.Equal(t, approved, replay)
	now = now.Add(ReceiptRetention)
	_, e = s.Get(ctx, "owner", active.ID)
	require.ErrorIs(t, e, ErrNotFound)
}
func TestConcurrentApprovalRetriesProduceOneReceipt(t *testing.T) {
	s, _ := testService(t)
	ctx := context.Background()
	i, e := s.Prepare(ctx, "owner", testInput(), "prepare-concurrent")
	require.NoError(t, e)
	var wg sync.WaitGroup
	results := make(chan *Intent, 20)
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := s.Approve(ctx, "owner", i.ID, i.Version, i.ActionHash, "approve-concurrent")
			if e != nil {
				errs <- e
			} else {
				results <- v
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	require.Empty(t, errs)
	receipt := ""
	for got := range results {
		if receipt == "" {
			receipt = got.Receipt.ID
		}
		require.Equal(t, receipt, got.Receipt.ID)
		require.Equal(t, int64(2), got.Version)
	}
}

type lostAcknowledgement struct {
	Store
	fail bool
}

func (s *lostAcknowledgement) CompareAndSwap(ctx context.Context, i *Intent, v int64) error {
	e := s.Store.CompareAndSwap(ctx, i, v)
	if e == nil && s.fail {
		s.fail = false
		return errors.New("response lost after write")
	}
	return e
}
func TestApprovalRecoversFromLostWriteAcknowledgement(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	i, e := s.Prepare(ctx, "owner", testInput(), "prepare-lost-ack")
	require.NoError(t, e)
	s.store = &lostAcknowledgement{Store: st, fail: true}
	_, e = s.Approve(ctx, "owner", i.ID, i.Version, i.ActionHash, "approve-lost-ack")
	require.Error(t, e)
	recovered, e := s.Approve(ctx, "owner", i.ID, i.Version, i.ActionHash, "approve-lost-ack")
	require.NoError(t, e)
	require.Equal(t, int64(2), recovered.Version)
	require.Equal(t, StatusApprovedBlocked, recovered.Status)
}
func TestPreviewVerificationIsExplicitlyNotRealFleet(t *testing.T) {
	_, st := testService(t)
	s := NewPreviewService(st)
	ctx := context.Background()
	options, e := s.Options(ctx)
	require.NoError(t, e)
	require.False(t, options.Capabilities.Execute)
	require.False(t, options.Capabilities.FleetVerified)
	require.Equal(t, "preview_fixture", options.Capabilities.CatalogSource)
	in := testInput()
	in.Repo = "preview/example"
	in.Node = "PREVIEW_ONLY"
	i, e := s.Prepare(ctx, "preview", in, "prepare-preview")
	require.NoError(t, e)
	require.False(t, i.Verification.RepoVerified)
	require.False(t, i.Verification.NodeVerified)
	other := NewService(st, testCatalog{})
	_, e = other.Approve(ctx, "preview", i.ID, i.Version, i.ActionHash, "approve-migrated")
	require.ErrorIs(t, e, ErrConflict)
}
func TestFileStoreRejectsConcurrentOpenAndCorruptedReview(t *testing.T) {
	s, st := testService(t)
	_, e := NewFileStore(st.path)
	require.Error(t, e)
	i, e := s.Prepare(context.Background(), "owner", testInput(), "prepare-corruption")
	require.NoError(t, e)
	st.mu.Lock()
	broken := st.records[recordKey("owner", i.ID)]
	broken.Action.Deploy = true
	st.records[recordKey("owner", i.ID)] = broken
	st.mu.Unlock()
	_, e = s.Get(context.Background(), "owner", i.ID)
	require.ErrorIs(t, e, ErrCorrupt)
}

type replaceDuringApproval struct {
	Store
	replace func(*Intent)
}

func (s *replaceDuringApproval) CompareAndSwap(_ context.Context, i *Intent, _ int64) error {
	s.replace(i)
	return ErrConflict
}

func TestApprovalConflictCannotReturnReceiptForRecreatedIntent(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	original, e := s.Prepare(ctx, "owner", testInput(), "prepare-replaced")
	require.NoError(t, e)
	// Simulate TTL removal and a new preparation and approval after our load,
	// but before our conditional write. The replacement deliberately reuses
	// the intent ID, request ID, and reviewed version: only its hash differs.
	s.store = &replaceDuringApproval{Store: st, replace: func(candidate *Intent) {
		replacement := cloneIntent(*candidate)
		replacement.Generation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if replacement.Generation == original.Generation {
			replacement.Generation = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}
		replacement.Action.Instructions = "A different action prepared after removal"
		replacement.ActionHash = actionHash(replacement.UserID, replacement.Verification.Source, replacement.Generation, replacement.Action)
		replacement.Receipt.ActionHash = replacement.ActionHash
		st.mu.Lock()
		st.records[recordKey(replacement.UserID, replacement.ID)] = replacement
		st.mu.Unlock()
	}}
	got, e := s.Approve(ctx, "owner", original.ID, original.Version, original.ActionHash, "approve-replaced")
	require.ErrorIs(t, e, ErrConflict)
	require.Nil(t, got)
	replacement, e := s.Get(ctx, "owner", original.ID)
	require.NoError(t, e)
	require.NotEqual(t, original.ActionHash, replacement.ActionHash)
	require.Equal(t, "approve-replaced", replacement.Receipt.RequestID)
	require.Equal(t, original.Version, replacement.Receipt.IntentVersion)
}

func TestFileStoreCASRejectsRecreatedIntentWithSameVersion(t *testing.T) {
	s, st := testService(t)
	ctx := context.Background()
	original, e := s.Prepare(ctx, "owner", testInput(), "prepare-local-aba")
	require.NoError(t, e)
	staleApproval, e := s.Approve(ctx, "owner", original.ID, original.Version, original.ActionHash, "approve-local-aba")
	require.NoError(t, e)
	// Restore the key as a newer prepared generation with the same version.
	// An old in-flight approval must fail the adapter's atomic hash fence.
	recreated := cloneIntent(*original)
	recreated.Generation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if recreated.Generation == original.Generation {
		recreated.Generation = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	}
	recreated.ActionHash = actionHash(recreated.UserID, recreated.Verification.Source, recreated.Generation, recreated.Action)
	st.mu.Lock()
	st.records[recordKey(recreated.UserID, recreated.ID)] = recreated
	st.mu.Unlock()
	require.ErrorIs(t, st.CompareAndSwap(ctx, staleApproval, original.Version), ErrConflict)
	current, e := s.Get(ctx, "owner", original.ID)
	require.NoError(t, e)
	require.Equal(t, recreated, *current)
}

func TestCapabilitiesRequireConfiguredCatalogAndOnlyFetchedOptionsVerifyFleet(t *testing.T) {
	_, st := testService(t)
	var missingGhost *ghost.Client
	for _, tc := range []struct {
		name    string
		catalog Catalog
	}{
		{name: "nil catalog"},
		{name: "typed nil Ghost", catalog: missingGhost},
		{name: "unconfigured Ghost", catalog: ghost.New(ghost.Config{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(st, tc.catalog)
			capabilities := s.Capabilities()
			require.False(t, capabilities.Prepare)
			require.False(t, capabilities.FleetVerified)
			require.False(t, capabilities.Execute)
			require.True(t, capabilities.Approve, "existing reviews remain accessible")
			require.Contains(t, capabilities.Message, "new reviews cannot be prepared")
			_, e := s.Options(context.Background())
			require.ErrorIs(t, e, ErrUnavailable)
			_, e = s.Prepare(context.Background(), "owner", testInput(), "prepare-unconfigured")
			require.ErrorIs(t, e, ErrUnavailable)
		})
	}
	s := NewService(st, testCatalog{})
	require.True(t, s.Capabilities().Prepare)
	require.False(t, s.Capabilities().FleetVerified, "configuration alone does not verify fleet access")
	options, e := s.Options(context.Background())
	require.NoError(t, e)
	require.True(t, options.Capabilities.FleetVerified)
	require.False(t, options.Capabilities.Execute)
	s.catalog = testCatalog{err: errors.New("provider offline")}
	_, e = s.Options(context.Background())
	require.ErrorIs(t, e, ErrUnavailable)
	require.False(t, s.Capabilities().FleetVerified, "a prior success is not retained as current verification")
	withoutStore := NewService(nil, testCatalog{})
	require.False(t, withoutStore.Capabilities().Prepare)
	require.False(t, withoutStore.Capabilities().Approve)
}
