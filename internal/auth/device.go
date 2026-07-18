// This file owns the M5Stack device-pairing lineage (plan.md M1 "Device
// 10-yr flow", M5 §6): the PAIR#<nonce> nonce lifecycle a bare device uses
// to acquire its permanent credentials, the PKCE (S256) proof that binds
// the eventual claim to the same physical device that started pairing, the
// creation of the device's 10-year refresh-token family, the ProvisionIoT
// integration seam M5 fills in with a real IoT Thing/cert, and the silent
// 24h rotation entry point steady-state device check-ins use.
//
// Route mapping (contracts/api.md — authoritative over plan.md's M1 prose
// shorthand):
//   - POST /auth/device/pair/start   -> RegisterPairing
//   - GET  /auth/device/pair/poll    -> PollPairing
//   - GET  /auth/lwa/device/callback -> serves the user-code confirm page
//     (LWA code-exchange + two-check Validate + Authorize preview; that
//     HTTP glue belongs to the web-core/auth route handler, not here)
//   - POST /auth/device/pair/confirm -> BindPairing (called once the human
//     has typed the RFC 8628 user code shown on the device's screen)
//   - GET  /auth/device/pair/claim   -> ClaimPairing
//   - POST /auth/refresh (surface=device) -> RotateDeviceSession
//
// Pairing flow, end to end:
//
//  1. The device calls RegisterPairing with the S256 code_challenge it
//     generated on-chip. A PAIR row is created (store.CreatePair defaults
//     it to status "pending", 15-minute TTL) carrying both the challenge
//     and a freshly generated RFC 8628-style user code (8 chars from the
//     "BCDFGHJKLMNPQRSTVWXZ" alphabet) that the device displays on its
//     screen as "XXXX-XXXX".
//
//  2. A human completes LWA sign-in in a phone/laptop browser, landing on
//     GET /auth/lwa/device/callback with state=<nonce>. That handler
//     serves a confirm page where the human must type the user code shown
//     on the device's screen, then calls BindPairing with it. BindPairing
//     re-runs the exact same Authorize gate every other sign-in surface
//     goes through (owner-binds-first, else owner match or allowlist, else
//     ErrNotAllowed), then requires a constant-time match of the presented
//     user code against the one stored on the PAIR row — the anti-phishing
//     proof that the human is looking at THIS device's screen, so an
//     attacker cannot phish a victim into binding the attacker's device to
//     the victim's account. A wrong code is counted (atomic increment);
//     after MaxUserCodeAttempts wrong entries the pairing is invalidated
//     (pending -> failed) and the device's poll sees a terminal "failed"
//     status telling it to restart pairing. Only when both gates pass does
//     BindPairing create the permanent DEVICE# record (provisioning IoT
//     via the ProvisionIoT hook, minting a fresh FamilyID that will anchor
//     the device's whole 10-year credential lineage) and atomically flip
//     the PAIR row pending -> bound, recording deviceId + userId on it.
//
//  3. The device itself keeps polling GET /auth/device/pair/poll (->
//     PollPairing, read-only) until it observes status "bound".
//
//  4. The device calls GET /auth/device/pair/claim with its original
//     code_verifier. ClaimPairing checks SHA256(code_verifier) against the
//     stored code_challenge (constant-time compare) — only on success does
//     it mint the device's first SESSION row (a fresh DeviceWindow/10-year
//     refresh token, generated here and returned in plaintext for the
//     first and only time — only its SHA-256 hash is ever written to
//     DynamoDB) plus hand back the IoT provisioning claim, then atomically
//     flips the PAIR row bound -> claimed so a replayed/racing claim call
//     fails closed with ErrPairAlreadyClaimed rather than handing out a
//     second credential for the same pairing.
//
//     Note: contracts/api.md's prose describes the callback (step 2) as
//     minting "the 10-year refresh family" — here that step mints the
//     family's identity (DEVICE.familyId) while the actual SESSION row and
//     its one plaintext refresh token are minted at step 4 instead. The
//     PAIR item (dictated shape: status/deviceId/userId/codeChallenge/
//     createdAt/ttl) has no field to carry a plaintext credential from
//     step 2 to step 4, so deferring generation to the PKCE-verified claim
//     is both spec-conformant (no new field) and strictly safer (the
//     refresh token plaintext is never written anywhere, even transiently).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/JeremyProffittOrg/live-ninja/internal/store"
	"github.com/google/uuid"
)

// PairTTL documents how long an unclaimed PAIR#<nonce> row lives before
// DynamoDB's TTL sweep reclaims it (store.CreatePair defaults new rows to
// this same 15-minute window — kept here too so handlers/docs have a
// single named constant to report to clients without reaching into the
// store package).
const PairTTL = 15 * time.Minute

// RFC 8628-style user-code parameters. The alphabet is RFC 8628 §6.1's
// recommended 20-consonant base-20 set (no vowels — cannot spell words; no
// easily-confused characters). 8 chars = 20^8 ≈ 2.56e10 combinations
// (~34.6 bits), far beyond what MaxUserCodeAttempts guesses could search.
const (
	// UserCodeAlphabet is the RFC 8628 §6.1 recommended character set.
	UserCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"
	// UserCodeLength is the number of alphabet characters in a user code
	// (stored/compared undashed; displayed as "XXXX-XXXX").
	UserCodeLength = 8
	// MaxUserCodeAttempts is how many wrong user-code entries invalidate
	// the pairing (pending -> failed) — the device must restart pairing.
	MaxUserCodeAttempts = 5
)

// Errors returned by the pairing lifecycle functions in this file.
var (
	// ErrPairNotFound means the nonce has no PAIR row — never registered,
	// already expired (TTL swept), or simply wrong.
	ErrPairNotFound = errors.New("auth: pairing nonce not found or expired")
	// ErrPairNotBound means ClaimPairing was called before the browser leg
	// (BindPairing) has completed — the device should keep polling.
	ErrPairNotBound = errors.New("auth: pairing not yet bound to a user")
	// ErrPairAlreadyClaimed means the PAIR row is past the point this call
	// is valid for: BindPairing sees a row that isn't "pending" anymore, or
	// ClaimPairing sees a row that's already "claimed" — including losing
	// the bound->claimed (or pending->bound) compare-and-swap to a
	// racing/replayed request.
	ErrPairAlreadyClaimed = errors.New("auth: pairing already bound/claimed")
	// ErrPKCEMismatch means the presented code_verifier does not hash (S256)
	// to the code_challenge recorded at RegisterPairing time — the caller
	// claiming this nonce is not the device that started pairing.
	ErrPKCEMismatch = errors.New("auth: pkce code_verifier does not match code_challenge")
	// ErrUserCodeMismatch is the errors.Is target for a wrong user-code
	// entry that has NOT yet exhausted the attempt budget — BindPairing
	// returns it wrapped in a *UserCodeMismatchError carrying the remaining
	// attempt count so the confirm page can tell the human how many tries
	// are left.
	ErrUserCodeMismatch = errors.New("auth: pairing user code does not match")
	// ErrPairFailed means the pairing was invalidated after too many wrong
	// user-code entries (status "failed") — terminal for this nonce; the
	// device must restart pairing from scratch.
	ErrPairFailed = errors.New("auth: pairing invalidated after too many incorrect user codes")
)

// UserCodeMismatchError is the concrete error BindPairing returns for a
// wrong (but not yet attempt-exhausting) user code. errors.Is(err,
// ErrUserCodeMismatch) matches it; AttemptsRemaining is how many more
// tries this pairing nonce will accept before it is invalidated.
type UserCodeMismatchError struct {
	AttemptsRemaining int
}

func (e *UserCodeMismatchError) Error() string {
	return fmt.Sprintf("%s (%d attempts remaining)", ErrUserCodeMismatch.Error(), e.AttemptsRemaining)
}

// Is makes errors.Is(err, ErrUserCodeMismatch) succeed on this type.
func (e *UserCodeMismatchError) Is(target error) bool { return target == ErrUserCodeMismatch }

// PairingClaim is the one-shot payload GET /auth/device/pair/claim hands
// back to the device: its permanent 10-year-family refresh token plus
// whatever IoT provisioning material ProvisionIoT produced. RefreshToken is
// shown in plaintext exactly once — the caller (the device) is responsible
// for persisting it (encrypted NVS, per M5 §6) since neither this function
// nor the store will ever return it again.
type PairingClaim struct {
	DeviceID     string
	UserID       string
	SessionID    string
	FamilyID     string
	RefreshToken string
	ExpiresAt    int64 // unix seconds
	ThingName    string
	CertArn      string
}

// ProvisionIoT is the M5 integration seam (plan.md M1 device.go task:
// "ProvisionIoT hook var (default returns empty thingName/certArn,
// logged) so M5 fills it. THIS IS the documented integration point, not a
// stub of required behavior."). Minting a per-device IoT Thing + X.509
// on-chip-keypair certificate lineage is M5's real work (plan.md M5 "IoT
// Core" + "10-yr persistence" tasks); until that lands, BindPairing still
// does everything else for real — authorization, DEVICE# record, 10-year
// refresh-token family — the device simply has no MQTT control-plane
// identity yet, which is honestly logged rather than silently faked.
var ProvisionIoT = func(ctx context.Context, log *slog.Logger, deviceID, userID string) (thingName, certArn string, err error) {
	if log != nil {
		log.InfoContext(ctx, "auth: IoT thing/cert provisioning deferred to M5",
			"deviceId", deviceID, "userId", userID)
	}
	return "", "", nil
}

// RegisterPairing implements POST /auth/device/pair/start: a bare M5Stack
// device, before it holds any Live Ninja credentials, registers a
// single-use PAIR#<nonce> row carrying the S256 PKCE code_challenge it
// generated on-chip plus a freshly generated RFC 8628-style user code. The
// nonce is what the device encodes into the QR/pairing-code a human scans
// to complete the browser leg; the user code (returned undashed — display
// it via FormatUserCode) is what the device shows on its screen for the
// human to type into the browser confirm page, proving the pairing they
// are approving belongs to the device in front of them.
func RegisterPairing(ctx context.Context, st *store.Store, codeChallenge string) (nonce, userCode string, expiresAt time.Time, err error) {
	if st == nil {
		return "", "", time.Time{}, errors.New("auth: store is required")
	}
	if codeChallenge == "" {
		return "", "", time.Time{}, errors.New("auth: code_challenge is required")
	}

	nonce, err = randomNonce()
	if err != nil {
		return "", "", time.Time{}, err
	}
	userCode, err = generateUserCode()
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("auth: generate user code: %w", err)
	}

	pair := &store.Pair{
		Nonce:         nonce,
		CodeChallenge: codeChallenge,
		UserCode:      userCode,
	}
	if err := st.CreatePair(ctx, pair); err != nil {
		return "", "", time.Time{}, fmt.Errorf("auth: create pairing: %w", err)
	}
	return nonce, userCode, time.Unix(pair.TTL, 0).UTC(), nil
}

// PollPairing implements GET /auth/device/pair/poll?nonce=<nonce>: the
// device polls this while a human completes the browser leg elsewhere.
// Read-only — never mutates the PAIR row. Callers should surface only
// pair.Status to the (still fully unauthenticated) device — RefreshToken
// and the identity fields are for BindPairing/ClaimPairing's own use, not
// for echoing back here.
func PollPairing(ctx context.Context, st *store.Store, nonce string) (*store.Pair, error) {
	if st == nil {
		return nil, errors.New("auth: store is required")
	}
	pair, err := st.GetPair(ctx, nonce)
	if err != nil {
		return nil, fmt.Errorf("auth: get pairing: %w", err)
	}
	if pair == nil {
		return nil, ErrPairNotFound
	}
	return pair, nil
}

// BindPairing implements the backend-side completion of GET
// /auth/lwa/device/callback (contracts/api.md): once that handler has run
// the browser leg's LWA code exchange and two-check Validate, it calls
// this with the resolved profile and the nonce carried in `state`.
//
// BindPairing runs the exact same access-control gate as every other
// sign-in surface (Authorize — first sign-in binds the owner; otherwise
// owner match or allowlist; otherwise ErrNotAllowed) before it does
// anything device-specific, so a device can never be paired to an account
// that couldn't sign in through the web/Android flow either. It then
// requires the RFC 8628 user code shown on the device's screen: userCode
// is normalized (case-insensitive, dash/space-insensitive) and compared in
// constant time against the code stored at RegisterPairing time. A
// mismatch counts one attempt (atomic increment) and returns a
// *UserCodeMismatchError; the MaxUserCodeAttempts'th wrong entry
// invalidates the pairing (pending -> failed, terminal — ErrPairFailed).
// The code is REQUIRED: there is no bind path that skips it. Only when
// both gates pass does it create the permanent DEVICE# record (FamilyID
// fresh, anchoring the whole 10-year credential lineage this device will
// ever have) and provision IoT (ProvisionIoT). The PAIR row is atomically
// flipped pending -> bound, recording deviceId+userId; ClaimPairing mints
// the actual SESSION/refresh-token once the device proves itself via PKCE.
func BindPairing(ctx context.Context, st *store.Store, log *slog.Logger, nonce, deviceName, userCode string, profile *LWAProfile) error {
	if st == nil {
		return errors.New("auth: store is required")
	}

	pair, err := st.GetPair(ctx, nonce)
	if err != nil {
		return fmt.Errorf("auth: get pairing: %w", err)
	}
	if pair == nil {
		return ErrPairNotFound
	}
	switch pair.Status {
	case store.PairStatusPending:
		// proceed
	case store.PairStatusFailed:
		return ErrPairFailed
	default:
		return ErrPairAlreadyClaimed
	}

	user, err := Authorize(ctx, st, profile)
	if err != nil {
		return err // ErrNotAllowed (or a wrapped store error) propagates as-is
	}

	// Anti-phishing user-code gate (RFC 8628 §5.4). matchUserCode fails
	// closed on an empty stored code, so a PAIR row that somehow lacked one
	// could never bind. Runs after Authorize so strangers can't burn a
	// pairing's attempt budget.
	if !matchUserCode(userCode, pair.UserCode) {
		return recordUserCodeFailure(ctx, st, log, nonce)
	}

	if deviceName == "" {
		deviceName = "Live Ninja Device"
	}

	deviceID := uuid.NewString()
	familyID := uuid.NewString()

	thingName, certArn, err := ProvisionIoT(ctx, log, deviceID, user.UserID)
	if err != nil {
		return fmt.Errorf("auth: provision iot: %w", err)
	}

	device := &store.Device{
		DeviceID:  deviceID,
		UserID:    user.UserID,
		Name:      deviceName,
		ThingName: thingName,
		CertArn:   certArn,
		Status:    store.DeviceStatusActive,
		FamilyID:  familyID,
		// CreatedAt intentionally left zero-value: CreateDevice fills it
		// with its own now-timestamp when unset, matching every other
		// Create* store method's established defaulting convention.
	}
	if err := st.CreateDevice(ctx, device); err != nil {
		return fmt.Errorf("auth: create device: %w", err)
	}

	if err := st.UpdatePair(ctx, nonce, store.PairStatusPending, store.PairStatusBound, deviceID, user.UserID); err != nil {
		if errors.Is(err, store.ErrInvalidPairState) {
			// Someone else (a racing duplicate callback, or a replay) bound
			// this nonce first. The DEVICE row we just created above is
			// simply orphaned — harmless in the common case (a duplicate
			// LWA callback for the same nonce lands on the already-bound
			// row); a periodic cleanup sweep for devices whose pairing
			// never reaches "claimed" is out of scope here.
			return ErrPairAlreadyClaimed
		}
		return fmt.Errorf("auth: bind pairing: %w", err)
	}

	if log != nil {
		log.InfoContext(ctx, "auth: device pairing bound",
			"nonce", nonce, "deviceId", deviceID, "userId", user.UserID, "familyId", familyID)
	}
	return nil
}

// ClaimPairing implements the single-use GET /auth/device/pair/claim: the
// device presents the PKCE code_verifier matching the code_challenge it
// registered in RegisterPairing. Only once that proof succeeds
// (constant-time S256 compare) does this mint the device's SESSION row —
// the first member of its 10-year (DeviceWindow) refresh-token family —
// and return the plaintext refresh token for the first and only time,
// alongside the device's IoT provisioning claim. The PAIR row is
// atomically flipped bound -> claimed so a replayed or racing claim
// request fails closed with ErrPairAlreadyClaimed rather than minting a
// second credential for the same pairing.
func ClaimPairing(ctx context.Context, st *store.Store, nonce, codeVerifier string) (*PairingClaim, error) {
	if st == nil {
		return nil, errors.New("auth: store is required")
	}
	if codeVerifier == "" {
		return nil, errors.New("auth: code_verifier is required")
	}

	pair, err := st.GetPair(ctx, nonce)
	if err != nil {
		return nil, fmt.Errorf("auth: get pairing: %w", err)
	}
	if pair == nil {
		return nil, ErrPairNotFound
	}
	switch pair.Status {
	case store.PairStatusPending:
		return nil, ErrPairNotBound
	case store.PairStatusClaimed:
		return nil, ErrPairAlreadyClaimed
	case store.PairStatusFailed:
		return nil, ErrPairFailed
	case store.PairStatusBound:
		// proceed
	default:
		return nil, fmt.Errorf("auth: pairing %q has unexpected status %q", nonce, pair.Status)
	}

	if !verifyPKCE(codeVerifier, pair.CodeChallenge) {
		return nil, ErrPKCEMismatch
	}

	device, err := st.GetDevice(ctx, pair.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("auth: get device: %w", err)
	}
	if device == nil {
		return nil, fmt.Errorf("auth: device %q from bound pairing %q not found", pair.DeviceID, nonce)
	}

	refreshToken, refreshHash, err := GenerateRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("auth: generate device refresh token: %w", err)
	}

	sessionID := uuid.NewString()
	expiresAt := time.Now().UTC().Add(DeviceWindow)

	sess := &store.Session{
		SessionID:   sessionID,
		UserID:      device.UserID,
		FamilyID:    device.FamilyID,
		Surface:     store.SurfaceDevice,
		DeviceID:    device.DeviceID,
		RefreshHash: refreshHash,
		ExpiresAt:   expiresAt.Unix(),
		TTL:         expiresAt.Unix(),
	}
	if err := st.CreateSession(ctx, sess); err != nil {
		return nil, fmt.Errorf("auth: create device session: %w", err)
	}

	if err := st.UpdatePair(ctx, nonce, store.PairStatusBound, store.PairStatusClaimed, "", ""); err != nil {
		if errors.Is(err, store.ErrInvalidPairState) {
			return nil, ErrPairAlreadyClaimed
		}
		return nil, fmt.Errorf("auth: finalize pairing claim: %w", err)
	}

	return &PairingClaim{
		DeviceID:     device.DeviceID,
		UserID:       device.UserID,
		SessionID:    sessionID,
		FamilyID:     device.FamilyID,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt.Unix(),
		ThingName:    device.ThingName,
		CertArn:      device.CertArn,
	}, nil
}

// RotateDeviceSession is the device-surface entry point the shared
// POST /auth/refresh handler calls when the presented session's surface is
// "device": it rotates the refresh token through the exact same
// reuse-detecting TransactWriteItems path every other surface uses
// (store.RotateRefresh — a presented hash matching prevHash revokes the
// whole family and returns store.ErrRefreshReuse), but slides
// expiresAt/ttl by the 10-year DeviceWindow instead of the 30-day
// SlidingWindow web/Android sessions use. M5 firmware calls this roughly
// every 24h (plan.md M5 "steady-state 24h mTLS refresh") so a device that
// never misses a check-in effectively never re-pairs, while a stolen/
// cloned refresh token is still caught the first time both the real device
// and the clone try to use it.
func RotateDeviceSession(ctx context.Context, st *store.Store, sess *store.Session, presentedRefreshToken string) (rotated *store.Session, newRefreshToken string, err error) {
	if st == nil {
		return nil, "", errors.New("auth: store is required")
	}
	if sess == nil {
		return nil, "", errors.New("auth: session is required")
	}
	if presentedRefreshToken == "" {
		return nil, "", errors.New("auth: refresh token is required")
	}

	presentedHash := HashRefreshToken(presentedRefreshToken)
	newRefreshToken, newHash, err := GenerateRefreshToken()
	if err != nil {
		return nil, "", fmt.Errorf("auth: generate device refresh token: %w", err)
	}
	slideTo := time.Now().UTC().Add(DeviceWindow).Unix()

	rotated, err = st.RotateRefresh(ctx, sess, presentedHash, newHash, slideTo)
	if err != nil {
		return nil, "", err // store.ErrRefreshReuse / store.ErrInvalidRefresh propagate
	}
	return rotated, newRefreshToken, nil
}

// randomNonce returns 32 cryptographically-random bytes, base64url
// (unpadded) encoded — the PAIR#<nonce> identifier. Distinct from
// GenerateRefreshToken (refresh.go) even though the shape is identical: a
// pairing nonce is a short-lived, single-use *identifier* embedded in a
// QR/pairing code, never itself the long-lived credential.
func randomNonce() (string, error) {
	return randomToken(32)
}

// verifyPKCE reports whether codeVerifier hashes (RFC 7636 S256) to
// codeChallenge, using a constant-time comparison so nonce-guessing or
// challenge-guessing timing side channels aren't a viable attack surface.
func verifyPKCE(codeVerifier, codeChallenge string) bool {
	sum := sha256.Sum256([]byte(codeVerifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(codeChallenge)) == 1
}

// generateUserCode returns UserCodeLength cryptographically-random
// characters from UserCodeAlphabet, using rejection sampling so every
// character is uniformly likely (256 % 20 != 0, so a bare modulo would
// bias toward the alphabet's early consonants).
func generateUserCode() (string, error) {
	// Largest multiple of len(UserCodeAlphabet) that fits in a byte:
	// bytes >= limit are rejected to kill the modulo bias.
	const limit = byte(256 - (256 % len(UserCodeAlphabet))) // 240
	out := make([]byte, 0, UserCodeLength)
	buf := make([]byte, 2*UserCodeLength)
	for len(out) < UserCodeLength {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b >= limit {
				continue
			}
			out = append(out, UserCodeAlphabet[int(b)%len(UserCodeAlphabet)])
			if len(out) == UserCodeLength {
				break
			}
		}
	}
	return string(out), nil
}

// FormatUserCode renders a stored (undashed) user code for humans:
// "GQNSVBTX" -> "GQNS-VBTX" (RFC 8628 §6.1 display form). Codes of an
// unexpected length pass through unchanged.
func FormatUserCode(code string) string {
	if len(code) != UserCodeLength {
		return code
	}
	return code[:UserCodeLength/2] + "-" + code[UserCodeLength/2:]
}

// NormalizeUserCode maps whatever a human typed onto the stored form:
// uppercased, with dashes and whitespace stripped — so "gqns-vbtx",
// "GQNS VBTX" and "gqnsvbtx" all compare equal to "GQNSVBTX".
func NormalizeUserCode(input string) string {
	var b strings.Builder
	b.Grow(len(input))
	for _, r := range strings.ToUpper(input) {
		switch r {
		case '-', ' ', '\t':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// matchUserCode reports whether the human-typed input matches the stored
// user code, in constant time (crypto/subtle) so the confirm endpoint
// can't be used as a per-character timing oracle. Fails closed when the
// stored code is empty — a pairing without a user code can never bind.
func matchUserCode(input, stored string) bool {
	if stored == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(NormalizeUserCode(input)), []byte(stored)) == 1
}

// recordUserCodeFailure books one wrong user-code entry against the
// pairing: atomic increment, then — once the attempt budget is spent —
// flips the pairing pending -> failed so the device's next poll sees a
// terminal status and restarts pairing. Returns *UserCodeMismatchError
// (errors.Is ErrUserCodeMismatch) while attempts remain, ErrPairFailed at
// exhaustion, ErrPairAlreadyClaimed when a racing bind/claim already moved
// the pairing out of pending.
func recordUserCodeFailure(ctx context.Context, st *store.Store, log *slog.Logger, nonce string) error {
	if err := st.IncrementPairAttempts(ctx, nonce); err != nil {
		if errors.Is(err, store.ErrInvalidPairState) {
			// The pairing left "pending" between our read and this write —
			// either a racing correct-code bind won (nonce spent) or a racing
			// wrong-code attempt already invalidated it.
			return ErrPairAlreadyClaimed
		}
		return fmt.Errorf("auth: record user code attempt: %w", err)
	}
	pair, err := st.GetPair(ctx, nonce)
	if err != nil {
		return fmt.Errorf("auth: get pairing after failed code: %w", err)
	}
	if pair == nil {
		return ErrPairNotFound
	}
	remaining := MaxUserCodeAttempts - pair.CodeAttempts
	if remaining > 0 {
		return &UserCodeMismatchError{AttemptsRemaining: remaining}
	}
	if err := st.UpdatePair(ctx, nonce, store.PairStatusPending, store.PairStatusFailed, "", ""); err != nil &&
		!errors.Is(err, store.ErrInvalidPairState) {
		// ErrInvalidPairState here means a racing attempt already flipped it
		// to failed (or a racing bind won — either way this nonce is done).
		return fmt.Errorf("auth: invalidate pairing: %w", err)
	}
	if log != nil {
		log.WarnContext(ctx, "auth: device pairing invalidated after too many wrong user codes",
			"nonce", nonce, "attempts", pair.CodeAttempts)
	}
	return ErrPairFailed
}
