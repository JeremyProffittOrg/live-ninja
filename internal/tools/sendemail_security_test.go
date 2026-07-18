package tools

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type capturingSQS struct{ sent int }

func (c *capturingSQS) SendMessage(ctx context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	c.sent++
	return &sqs.SendMessageOutput{}, nil
}

func invEmail(idk string, args map[string]any) Invocation {
	inv := invocation("send_email", args)
	inv.IdempotencyKey = idk
	return inv
}

// Regression for the send_email exfil finding (M1): the model's confirmExternal
// boolean is NOT sufficient — an external recipient must also be on the
// owner-managed access allow-list, else the send is refused server-side.
func TestSendEmailExternalRequiresAllowlist(t *testing.T) {
	deps := newTestDeps()
	sqsFake := &capturingSQS{}
	deps.SQS = sqsFake
	deps.EmailQueueURL = "https://sqs.example/q"
	deps.OwnerEmail = "owner@jeremy.ninja"
	r := newTestRegistry(t, deps)
	ctx := context.Background()

	// (a) External + confirmExternal=true but NOT allow-listed -> refused, nothing sent.
	res := r.Invoke(ctx, invEmail("k1", map[string]any{"to": "attacker@evil.com", "subject": "x", "body": "y", "confirmExternal": true}))
	require.False(t, res.OK, "unallowlisted external recipient must be refused")
	assert.Equal(t, 0, sqsFake.sent)

	// (b) Owner recipient -> always allowed.
	res = r.Invoke(ctx, invEmail("k2", map[string]any{"to": "owner@jeremy.ninja", "subject": "x", "body": "y"}))
	require.True(t, res.OK, "owner recipient must be allowed: %+v", res.Error)
	assert.Equal(t, 1, sqsFake.sent)

	// (c) Allow-listed external + confirmed -> allowed.
	require.NoError(t, deps.Store.AddAllow(ctx, "friend@jeremy.ninja", "owner"))
	res = r.Invoke(ctx, invEmail("k3", map[string]any{"to": "friend@jeremy.ninja", "subject": "x", "body": "y", "confirmExternal": true}))
	require.True(t, res.OK, "allow-listed external + confirmed must be allowed: %+v", res.Error)
	assert.Equal(t, 2, sqsFake.sent)
}
