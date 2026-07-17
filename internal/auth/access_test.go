package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
)

func TestAuthorizeFirstSignInBindsOwner(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()

	profile := &LWAProfile{UserID: "amzn1.account.first", Email: "First@Example.com", Name: "First"}
	user, err := Authorize(ctx, st, profile)
	require.NoError(t, err)
	assert.Equal(t, store.RoleOwner, user.Role)
	assert.Equal(t, "first@example.com", user.Email) // lowercased

	owner, err := st.GetOwner(ctx)
	require.NoError(t, err)
	require.NotNil(t, owner)
	assert.Equal(t, profile.UserID, owner.AmazonUserID)
	assert.Equal(t, user.UserID, owner.UserID)

	// Signing in again resolves the same user, still owner.
	again, err := Authorize(ctx, st, profile)
	require.NoError(t, err)
	assert.Equal(t, user.UserID, again.UserID)
}

func TestAuthorizeRejectsNonOwnerNonAllowlisted(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()

	_, err := Authorize(ctx, st, &LWAProfile{UserID: "amzn1.account.owner", Email: "o@example.com"})
	require.NoError(t, err)

	_, err = Authorize(ctx, st, &LWAProfile{UserID: "amzn1.account.other", Email: "other@example.com"})
	require.ErrorIs(t, err, ErrNotAllowed)
}

func TestAuthorizeAllowlistByEmailCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()

	_, err := Authorize(ctx, st, &LWAProfile{UserID: "amzn1.account.owner", Email: "o@example.com"})
	require.NoError(t, err)

	// Allowlist stores the lowercased email; sign-in presents mixed case.
	require.NoError(t, st.AddAllow(ctx, "Friend@Example.COM", "owner"))

	user, err := Authorize(ctx, st, &LWAProfile{UserID: "amzn1.account.friend", Email: "friend@EXAMPLE.com", Name: "Friend"})
	require.NoError(t, err)
	assert.Equal(t, store.RoleMember, user.Role)
}

func TestAuthorizeAllowlistByAmazonUserID(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()

	_, err := Authorize(ctx, st, &LWAProfile{UserID: "amzn1.account.owner", Email: "o@example.com"})
	require.NoError(t, err)

	require.NoError(t, st.AddAllow(ctx, "amzn1.account.friend2", "owner"))

	user, err := Authorize(ctx, st, &LWAProfile{UserID: "amzn1.account.friend2", Email: "whatever@example.com"})
	require.NoError(t, err)
	assert.Equal(t, store.RoleMember, user.Role)
}

func TestAuthorizeRemovedFromAllowlist(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()

	_, err := Authorize(ctx, st, &LWAProfile{UserID: "amzn1.account.owner", Email: "o@example.com"})
	require.NoError(t, err)
	require.NoError(t, st.AddAllow(ctx, "friend@example.com", "owner"))
	_, err = Authorize(ctx, st, &LWAProfile{UserID: "amzn1.account.f", Email: "friend@example.com"})
	require.NoError(t, err)

	require.NoError(t, st.RemoveAllow(ctx, "friend@example.com"))
	_, err = Authorize(ctx, st, &LWAProfile{UserID: "amzn1.account.f", Email: "friend@example.com"})
	require.ErrorIs(t, err, ErrNotAllowed)
}

func TestAuthorizeRejectsNilProfile(t *testing.T) {
	st := newFakeStore()
	_, err := Authorize(context.Background(), st, nil)
	require.Error(t, err)
	_, err = Authorize(context.Background(), st, &LWAProfile{})
	require.Error(t, err)
}
