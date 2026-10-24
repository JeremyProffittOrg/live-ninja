package tools

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
)

type suburbanStub struct {
	calls     int
	owner, id string
	failed    bool
}

func (s *suburbanStub) Invoke(_ context.Context, owner, name string, args map[string]any, id string) (map[string]any, error) {
	s.calls++
	s.owner, s.id = owner, id
	return map[string]any{"isError": s.failed, "requestId": id}, nil
}
func TestSuburbanOwnerIdentityAndIdempotency(t *testing.T) {
	d := newTestDeps()
	s := &suburbanStub{}
	d.Suburban = s
	r := newTestRegistry(t, d)
	i := invocation("suburban_remote", map[string]any{"tool": "vehicle_lock"})
	i.Role = "user"
	i.IdempotencyKey = "stable-intent"
	require.False(t, r.Invoke(context.Background(), i).OK)
	require.Zero(t, s.calls)
	i.Role = "owner"
	first := r.Invoke(context.Background(), i)
	require.True(t, first.OK)
	require.Equal(t, i.UserID, s.owner)
	require.Len(t, s.id, 64)
	second := r.Invoke(context.Background(), i)
	require.True(t, second.Duplicate)
	require.Equal(t, 1, s.calls)
	i.IdempotencyKey = "another-intent"
	i.Args["argumentsJSON"] = `[]`
	require.False(t, r.Invoke(context.Background(), i).OK)
	require.Equal(t, 1, s.calls)
	i.Args["argumentsJSON"] = `{}`
	i.IdempotencyKey = "rejected-intent"
	s.failed = true
	failed := r.Invoke(context.Background(), i)
	require.False(t, failed.OK)
	require.Equal(t, CodeUpstreamError, failed.Error.Code)
	require.NotEmpty(t, failed.Error.Details["requestId"])
}
