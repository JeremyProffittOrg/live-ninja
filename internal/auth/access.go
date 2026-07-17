package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/google/uuid"
)

// ErrNotAllowed is returned by Authorize when the validated LWA profile is
// neither the bound owner nor present on the admin-managed allowlist.
// Access rule (LOCKED, plan.md §1): first sign-in binds the owner; after
// that, only the owner and allowlisted identities may sign in — every
// other Amazon login is rejected with HTTP 403 by the caller.
var ErrNotAllowed = errors.New("auth: user not allowed")

const (
	roleOwner  = "owner"
	roleMember = "member"

	statusActive   = "active"
	statusDisabled = "disabled"
)

// Authorize implements the LOCKED access-control rule for every LWA
// sign-in (web callback, Android exchange, device browser leg): the
// first successful sign-in ever binds the CONFIG/OWNER record to that
// Amazon user_id (role=owner); every subsequent sign-in is accepted only
// if it matches the bound owner OR appears on the CONFIG/ALLOW# list
// (checked by both amazonUserId and lowercased email); anyone else gets
// ErrNotAllowed. On any accepted path the corresponding USER item is
// created if it doesn't exist yet (upsert-on-first-sign-in), and a
// disabled user is rejected even if they would otherwise match.
func Authorize(ctx context.Context, st *store.Store, profile *LWAProfile) (*store.User, error) {
	if st == nil {
		return nil, errors.New("auth: store is required")
	}
	if profile == nil || profile.UserID == "" {
		return nil, errors.New("auth: lwa profile is required")
	}

	owner, err := st.GetOwner(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: get owner: %w", err)
	}

	// Unbound: this sign-in becomes the owner. BindOwner is a
	// conditional write (attribute_not_exists(pk)), so a race between
	// two simultaneous "first" sign-ins is resolved by DynamoDB, not by
	// this function — the loser retries as a normal sign-in against
	// whichever identity actually won.
	if owner == nil {
		return bindOwnerAndUpsert(ctx, st, profile)
	}

	if owner.AmazonUserID == profile.UserID {
		return upsertUser(ctx, st, profile, roleOwner)
	}

	email := strings.ToLower(strings.TrimSpace(profile.Email))
	allowed, err := st.IsAllowed(ctx, profile.UserID, email)
	if err != nil {
		return nil, fmt.Errorf("auth: check allowlist: %w", err)
	}
	if !allowed {
		return nil, ErrNotAllowed
	}

	return upsertUser(ctx, st, profile, roleMember)
}

// bindOwnerAndUpsert handles the "no owner bound yet" branch of
// Authorize: create (or reuse) this profile's USER record, then
// conditionally bind CONFIG/OWNER to it.
func bindOwnerAndUpsert(ctx context.Context, st *store.Store, profile *LWAProfile) (*store.User, error) {
	user, err := getOrCreateUser(ctx, st, profile, roleOwner)
	if err != nil {
		return nil, err
	}
	if user.Status == statusDisabled {
		return nil, ErrNotAllowed
	}

	if err := st.BindOwner(ctx, profile.UserID, user.UserID); err != nil {
		if errors.Is(err, store.ErrAlreadyBound) {
			// Lost the race to become the first owner (a concurrent
			// sign-in bound CONFIG/OWNER first) — re-run Authorize from
			// scratch so this request is evaluated as an ordinary
			// sign-in against whichever identity actually won.
			return Authorize(ctx, st, profile)
		}
		return nil, fmt.Errorf("auth: bind owner: %w", err)
	}

	return user, nil
}

// upsertUser resolves the USER item for an already-authorized sign-in
// (owner match or allowlist match): reuse the existing record (rejecting
// a disabled account) or create a new one with the given role.
func upsertUser(ctx context.Context, st *store.Store, profile *LWAProfile, role string) (*store.User, error) {
	existing, err := st.GetUserByLWA(ctx, profile.UserID)
	if err != nil {
		return nil, fmt.Errorf("auth: get user by lwa: %w", err)
	}
	if existing != nil {
		if existing.Status == statusDisabled {
			return nil, ErrNotAllowed
		}
		return existing, nil
	}
	return getOrCreateUser(ctx, st, profile, role)
}

// getOrCreateUser fetches the USER item for profile.UserID, creating it
// (active, with the given role) if none exists yet. Callers that already
// hold a fresh "not found" result may call this directly (the
// first-sign-in-binds-owner path); upsertUser re-checks first to avoid a
// redundant lookup when it already confirmed absence isn't the case.
func getOrCreateUser(ctx context.Context, st *store.Store, profile *LWAProfile, role string) (*store.User, error) {
	existing, err := st.GetUserByLWA(ctx, profile.UserID)
	if err != nil {
		return nil, fmt.Errorf("auth: get user by lwa: %w", err)
	}
	if existing != nil {
		return existing, nil
	}

	user := &store.User{
		UserID:           uuid.NewString(),
		AmazonUserID:     profile.UserID,
		Email:            strings.ToLower(strings.TrimSpace(profile.Email)),
		Name:             profile.Name,
		Role:             role,
		Status:           statusActive,
		TokensValidAfter: 0,
		CreatedAt:        time.Now().Unix(),
	}
	if err := st.CreateUser(ctx, user); err != nil {
		return nil, fmt.Errorf("auth: create user: %w", err)
	}
	return user, nil
}
