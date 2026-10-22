package alexa

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/realtime"
	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
	"github.com/stretchr/testify/require"
)

func serviceFixture(t *testing.T) (*Service, Identity, *time.Time, *int, *int) {
	t.Helper()
	now := time.Now().UTC()
	published, ran := 0, 0
	id := Identity{Owner: "owner", Device: "echo", Grant: "grant", Generation: "first"}
	db := testutil.NewFakeDynamo()
	accounts := store.NewWithClient(db, "test")
	require.NoError(t, accounts.CreateUser(t.Context(), &store.User{UserID: id.Owner, AmazonUserID: "amazon-owner", Role: store.RoleOwner, Status: store.UserStatusActive}))
	require.NoError(t, accounts.BindOwner(t.Context(), "amazon-owner", id.Owner))
	s := &Service{Repo: NewDynamo(db, "test"), Now: func() time.Time { return now },
		Authorize: func(_ context.Context, a Identity) error {
			if a != id {
				return ErrUnauthorized
			}
			return nil
		},
		Settings: func(context.Context, Identity) (Settings, error) {
			return Settings{Version: 1, KeepTranscript: false, RetentionDays: 0}, nil
		},
		Publish: func(context.Context, Job) error { published++; return nil },
		Run: func(ctx context.Context, w Work) (RunResult, error) {
			ran++
			if err := w.Check(ctx); err != nil {
				return RunResult{}, err
			}
			return RunResult{Text: "Richmond", Messages: append(w.Messages, realtime.ChatMessage{Role: "assistant", Content: "Richmond"})}, nil
		},
	}
	return s, id, &now, &published, &ran
}

func TestContractDurableTurnReplayAndFollowup(t *testing.T) {
	s, id, now, published, ran := serviceFixture(t)
	ctx := t.Context()
	first, err := s.Submit(ctx, id, "request-1", "ask-session", "Which city is Virginia's capital?")
	require.NoError(t, err)
	require.Equal(t, "pending", first.State)
	duplicate, err := s.Submit(ctx, id, "request-1", "ask-session", "Which city is Virginia's capital?")
	require.NoError(t, err)
	require.Equal(t, first.Receipt, duplicate.Receipt)
	require.Equal(t, 1, *published)
	require.NoError(t, s.Execute(ctx, Job{Owner: id.Owner, Turn: first.Receipt}))
	require.NoError(t, s.Execute(ctx, Job{Owner: id.Owner, Turn: first.Receipt}))
	require.Equal(t, 1, *ran)
	view, err := s.Status(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "complete", view.State)
	require.Equal(t, "Richmond", view.Text)
	s.Run = func(ctx context.Context, w Work) (RunResult, error) {
		require.Len(t, w.Messages, 3)
		require.Equal(t, "Richmond", w.Messages[1].Content)
		require.Equal(t, "What about its population?", w.Messages[2].Content)
		return RunResult{Text: "A follow-up answer", Messages: append(w.Messages, realtime.ChatMessage{Role: "assistant", Content: "A follow-up answer"})}, nil
	}
	next, err := s.Submit(ctx, id, "request-2", "ask-session", "What about its population?")
	require.NoError(t, err)
	require.NoError(t, s.Execute(ctx, Job{Owner: id.Owner, Turn: next.Receipt}))
	*now = now.Add(16 * time.Minute)
	_, err = s.Status(ctx, id)
	require.ErrorIs(t, err, ErrExpired)
}

func TestAlexaBrokerAuthorityRequiresRunningReceipt(t *testing.T) {
	s, id, _, _, _ := serviceFixture(t)
	v, err := s.Submit(t.Context(), id, "request", "session", "Hello")
	require.NoError(t, err)
	job := Job{Owner: id.Owner, Turn: v.Receipt}
	_, err = s.CheckWork(t.Context(), job)
	require.Error(t, err, "pending receipt must not authorize model work")
	s.Run = func(ctx context.Context, w Work) (RunResult, error) {
		verified, e := s.CheckWork(ctx, job)
		require.NoError(t, e)
		require.Equal(t, id, verified.Identity)
		require.Equal(t, w.SessionID, verified.SessionID)
		require.True(t, verified.Deadline.After(time.Now()))
		require.NoError(t, s.Reset(ctx, id, "new-session", true))
		_, e = s.CheckWork(ctx, job)
		require.Error(t, e, "stop/reset must revoke broker authority")
		return RunResult{}, e
	}
	require.NoError(t, s.Execute(t.Context(), job))
}

func TestContractCancellationStopsToolsAndClearsContext(t *testing.T) {
	s, id, _, _, _ := serviceFixture(t)
	ctx := t.Context()
	view, err := s.Submit(ctx, id, "request-1", "ask-session", "Do some work")
	require.NoError(t, err)
	s.Run = func(ctx context.Context, w Work) (RunResult, error) {
		require.NoError(t, s.Reset(ctx, id, "", false))
		return RunResult{}, w.Check(ctx)
	}
	require.NoError(t, s.Execute(ctx, Job{Owner: id.Owner, Turn: view.Receipt}))
	_, err = s.Status(ctx, id)
	require.ErrorIs(t, err, ErrExpired)
}

func TestContractPublishFailureAndChangedSettingsNeverRepeatWork(t *testing.T) {
	s, id, _, _, ran := serviceFixture(t)
	ctx := t.Context()
	s.Publish = func(context.Context, Job) error { return errors.New("queue unavailable") }
	_, err := s.Submit(ctx, id, "failed", "session", "hello")
	require.Error(t, err)
	view, err := s.Status(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "failed", view.State)
	require.NoError(t, s.Execute(ctx, Job{Owner: id.Owner, Turn: view.Receipt}))
	require.Zero(t, *ran)
	s.Publish = func(context.Context, Job) error { return nil }
	view, err = s.Submit(ctx, id, "next", "session", "hello")
	require.NoError(t, err)
	s.Settings = func(context.Context, Identity) (Settings, error) { return Settings{Version: 2}, nil }
	require.NoError(t, s.Execute(ctx, Job{Owner: id.Owner, Turn: view.Receipt}))
	require.Zero(t, *ran)
	view, err = s.Status(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "cancelled", view.State)
}

func TestContractSingleOwnerConcurrencyAndExpiredWorkerCleanup(t *testing.T) {
	s, id, now, _, ran := serviceFixture(t)
	ctx := t.Context()
	first, err := s.Submit(ctx, id, "first", "session", "first private input")
	require.NoError(t, err)
	_, err = s.Submit(ctx, id, "other", "session", "another request")
	require.ErrorIs(t, err, ErrBusy)
	*now = now.Add(40 * time.Second)
	second, err := s.Submit(ctx, id, "next", "session", "second input")
	require.NoError(t, err)
	require.NoError(t, s.Execute(ctx, Job{Owner: id.Owner, Turn: first.Receipt}))
	require.Zero(t, *ran)
	require.NoError(t, s.Execute(ctx, Job{Owner: id.Owner, Turn: second.Receipt}))
	require.Equal(t, 1, *ran)
	// Explicit stop leaves no recoverable answer from either request.
	require.NoError(t, s.Reset(ctx, id, "", false))
	_, err = s.Status(ctx, id)
	require.ErrorIs(t, err, ErrExpired)
	db := s.Repo.(*Dynamo).DB.(*testutil.FakeDynamo)
	items, err := store.NewWithClient(db, "test").QueryUserPartition(ctx, id.Owner)
	require.NoError(t, err)
	for _, item := range items {
		if data, ok := item["data"].(string); ok {
			require.NotContains(t, data, "first private input")
			require.NotContains(t, data, "second input")
		}
	}
}

func TestContractWorkerCrashCannotReplayUncertainActions(t *testing.T) {
	s, id, now, _, _ := serviceFixture(t)
	view, err := s.Submit(t.Context(), id, "crash-request", "session", "Do the requested work")
	require.NoError(t, err)
	job := Job{Owner: id.Owner, Turn: view.Receipt}
	calls := 0
	s.Run = func(context.Context, Work) (RunResult, error) {
		calls++
		panic("worker process terminated after a possible side effect")
	}
	require.Panics(t, func() { _ = s.Execute(t.Context(), job) })
	require.NoError(t, s.Execute(t.Context(), job))
	require.Equal(t, 1, calls)
	*now = now.Add(40 * time.Second)
	status, err := s.Status(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "failed", status.State)
	require.Contains(t, status.Text, "will not repeat")
}

func TestContractAccountDeletionFencePreventsStateRecreation(t *testing.T) {
	s, id, _, _, _ := serviceFixture(t)
	db := s.Repo.(*Dynamo).DB.(*testutil.FakeDynamo)
	require.NoError(t, store.NewWithClient(db, "test").SuspendUser(t.Context(), id.Owner, "deletion test"))
	// The outer check was already granted; the storage transaction still refuses
	// the now-inactive account, as it must during an account purge race.
	_, err := s.Submit(t.Context(), id, "late", "session", "must not persist")
	require.ErrorIs(t, err, ErrUnauthorized)
}
