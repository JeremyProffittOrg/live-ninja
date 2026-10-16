package tools

import (
	"context"
	"strings"
	"testing"
)

type fakeOwnerPhone struct {
	owner, id, destination, message string
	sends                           int
}

func (p *fakeOwnerPhone) Send(_ context.Context, owner, id, destination, message string) (map[string]any, *ToolError) {
	p.owner = owner
	p.id = id
	p.destination = destination
	p.message = message
	p.sends++
	return map[string]any{"requestId": id, "accepted": true, "providerStatus": "queued"}, nil
}
func (p *fakeOwnerPhone) Status(_ context.Context, owner, id string) (map[string]any, *ToolError) {
	p.owner = owner
	p.id = id
	return map[string]any{"providerStatus": "completed", "humanHeard": "not_verified"}, nil
}
func TestOwnerPhoneDefinitionsAndStableIdentity(t *testing.T) {
	fake := &fakeOwnerPhone{}
	d := &Deps{Phone: fake}
	def := callMessageDefinition()
	if !def.OwnerOnly || !def.SideEffecting {
		t.Fatal("missing authorization/idempotency boundary")
	}
	args, e := validateArgs(def, map[string]any{"message": "Hello Jeremy."})
	if e != nil {
		t.Fatal(e)
	}
	inv := Invocation{UserID: "owner", IdempotencyKey: "stable-call"}
	for i := 0; i < 2; i++ {
		out, e := def.Handler(context.Background(), d, inv, args)
		if e != nil || out["providerStatus"] != "queued" {
			t.Fatal(out, e)
		}
	}
	if fake.owner != "owner" || fake.destination != "primary" || len(fake.id) != 64 || fake.message != "Hello Jeremy." {
		t.Fatal(fake)
	}
	firstID := fake.id
	inv.UserID = "different"
	def.Handler(context.Background(), d, inv, args)
	if fake.id == firstID {
		t.Fatal("request IDs cross owner boundary")
	}
	for _, args := range []map[string]any{{"destination": "+15555550123", "message": "Hello"}, {"message": strings.Repeat("x", 1001)}, {"message": ""}, {"message": "Hello", "ownerUserId": "other"}} {
		if _, e := validateArgs(def, args); e == nil {
			t.Fatal("invalid phone input accepted", args)
		}
	}
	status := phoneMessageStatusDefinition()
	if !status.OwnerOnly || status.SideEffecting {
		t.Fatal("status permissions")
	}
}
func TestOwnerPhoneMissingServiceFailsClosed(t *testing.T) {
	_, e := callMessageDefinition().Handler(context.Background(), &Deps{}, Invocation{}, map[string]any{"message": "Hello"})
	if e == nil || e.Code != CodeNotConfigured {
		t.Fatal(e)
	}
}
