package store

import "errors"

// Sentinel errors shared across the auth/session store surface. Handlers
// map these onto HTTP statuses (ErrRefreshReuse → family revoked + security
// alert; ErrInvalidRefresh → 401; ErrAlreadyBound → owner already claimed).
var (
	// ErrRefreshReuse is returned by RotateRefresh when the presented
	// refresh-token hash matches the session's *previous* hash — i.e. a
	// token that was already rotated is being replayed. By the time this
	// error is returned the whole session family has been revoked.
	ErrRefreshReuse = errors.New("store: refresh token reuse detected")

	// ErrInvalidRefresh is returned by RotateRefresh when the presented
	// hash matches neither the current nor the previous refresh hash.
	ErrInvalidRefresh = errors.New("store: invalid refresh token")

	// ErrAlreadyBound is returned by BindOwner when CONFIG/OWNER already
	// exists (first-sign-in-binds-owner lost the conditional write).
	ErrAlreadyBound = errors.New("store: owner already bound")

	// ErrInvalidPairState is returned by UpdatePair when the PAIR item is
	// absent, expired, or not in the expected status (single-use nonce
	// protection — a lost bind/claim race fails closed).
	ErrInvalidPairState = errors.New("store: pair nonce absent or not in expected state")

	// ErrNotFound is returned by targeted mutations (SetTokensValidAfter,
	// RevokeDevice) whose conditional attribute_exists check fails.
	ErrNotFound = errors.New("store: item not found")
)

// Enumerated attribute values (single source of truth for the strings that
// land in DynamoDB — see the dictated single-table item shapes).
const (
	RoleOwner  = "owner"
	RoleMember = "member"

	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"

	SurfaceWeb     = "web"
	SurfaceAndroid = "android"
	SurfaceDevice  = "device"

	PairStatusPending = "pending"
	PairStatusClaimed = "claimed"
	PairStatusBound   = "bound"
	PairStatusFailed  = "failed"

	DeviceStatusActive  = "active"
	DeviceStatusRevoked = "revoked"
)

// GSI index names as deployed in template.yaml (projection ALL on both).
const (
	indexGSI1 = "GSI1"
	indexGSI2 = "GSI2"
)

// User is the USER#<uid>/PROFILE item (GSI1: LWA#<amazonUserId>/PROFILE
// for reverse lookup from the LWA identity).
type User struct {
	UserID           string `dynamodbav:"userId"`
	AmazonUserID     string `dynamodbav:"amazonUserId"`
	Email            string `dynamodbav:"email"`
	Name             string `dynamodbav:"name"`
	Role             string `dynamodbav:"role"`   // owner | member
	Status           string `dynamodbav:"status"` // active | disabled
	TokensValidAfter int64  `dynamodbav:"tokensValidAfter"`
	CreatedAt        int64  `dynamodbav:"createdAt"` // unix seconds
}

// OwnerBinding is the CONFIG/OWNER singleton: the first successful
// sign-in binds the owner via a conditional Put (attribute_not_exists).
type OwnerBinding struct {
	AmazonUserID string `dynamodbav:"amazonUserId"`
	UserID       string `dynamodbav:"userId"`
	BoundAt      string `dynamodbav:"boundAt"` // RFC3339
}

// AllowEntry is one CONFIG/ALLOW#<amazonUserId-or-email-lowercase> row.
// Key carries the identifier without the ALLOW# prefix.
type AllowEntry struct {
	Key     string `dynamodbav:"-"`
	AddedBy string `dynamodbav:"addedBy"`
	AddedAt string `dynamodbav:"addedAt"` // RFC3339
}

// Session is the USER#<uid>/SESS#<sessionId> item. Only the SHA-256 hex
// hash of the refresh token is ever stored (RefreshHash current,
// PrevHash the immediately prior one — kept for reuse detection).
// GSI1: SESS#<sessionId>/SESS (lookup by sessionId);
// GSI2: USER#<uid>#SESS / <lastUsedAt RFC3339> (active-session feed).
type Session struct {
	SessionID   string `dynamodbav:"sessionId"`
	UserID      string `dynamodbav:"userId"`
	FamilyID    string `dynamodbav:"familyId"`
	Surface     string `dynamodbav:"surface"` // web | android | device
	DeviceID    string `dynamodbav:"deviceId,omitempty"`
	RefreshHash string `dynamodbav:"refreshHash"`
	PrevHash    string `dynamodbav:"prevHash,omitempty"`
	CreatedAt   int64  `dynamodbav:"createdAt"`  // unix seconds
	LastUsedAt  int64  `dynamodbav:"lastUsedAt"` // unix seconds (GSI2 sk carries the RFC3339 form)
	ExpiresAt   int64  `dynamodbav:"expiresAt"`  // unix seconds
	TTL         int64  `dynamodbav:"ttl"`        // unix seconds (DynamoDB TTL)
}

// OAuthState is the one-shot OAUTH#<state>/STATE item (10-minute TTL)
// holding the PKCE verifier between /auth/lwa/login and the callback.
type OAuthState struct {
	State        string `dynamodbav:"-"`
	CodeVerifier string `dynamodbav:"codeVerifier"`
	Surface      string `dynamodbav:"surface"`
	RedirectURI  string `dynamodbav:"redirectURI"`
	DeviceNonce  string `dynamodbav:"deviceNonce,omitempty"`
	// AppChallenge/AppState carry the Android broker flow through the shared
	// LWA callback: when AppChallenge is set, the callback hands a one-shot
	// handoff code back to the app via its custom scheme (PKCE-bound to
	// AppChallenge, replayed under AppState) instead of opening a web
	// session. See internal/webapp/auth_routes.go appLogin/completeAppHandoff.
	AppChallenge string `dynamodbav:"appChallenge,omitempty"`
	AppState     string `dynamodbav:"appState,omitempty"`
	CreatedAt    int64  `dynamodbav:"createdAt"` // unix seconds
	TTL          int64  `dynamodbav:"ttl"`       // unix seconds
}

// Pair is the PAIR#<nonce>/PAIR item (15-minute TTL) tracking the device
// pairing lifecycle: pending → bound → claimed, or pending → failed when
// the RFC 8628 user code is entered wrong too many times (see
// internal/auth/device.go for the full state machine).
type Pair struct {
	Nonce         string `dynamodbav:"-"`
	Status        string `dynamodbav:"status"` // pending | claimed | bound | failed
	DeviceID      string `dynamodbav:"deviceId,omitempty"`
	UserID        string `dynamodbav:"userId,omitempty"`
	CodeChallenge string `dynamodbav:"codeChallenge"`
	// UserCode is the RFC 8628-style anti-phishing code (8 chars from the
	// "BCDFGHJKLMNPQRSTVWXZ" alphabet, undashed) shown on the device's
	// screen; the browser confirm leg must present it before BindPairing
	// will bind the device to an account. Required at creation — a PAIR
	// row without a user code cannot exist.
	UserCode string `dynamodbav:"userCode"`
	// CodeAttempts counts wrong user-code entries (atomic ADD via
	// IncrementPairAttempts); the auth layer invalidates the pairing
	// (status=failed) when it reaches the max.
	CodeAttempts int   `dynamodbav:"codeAttempts,omitempty"`
	CreatedAt    int64 `dynamodbav:"createdAt"` // unix seconds
	TTL          int64 `dynamodbav:"ttl"`       // unix seconds
}

// PairConfirm is the PAIRCONFIRM#<token>/CONFIRM item (10-minute TTL): the
// browser confirm leg's short-lived link between a completed LWA sign-in
// and the user-code entry form. The token is held by the authenticated
// browser (hidden form field + HttpOnly cookie); the row carries the
// LWA-verified identity forward to the confirm POST, since the one-shot
// OAuth state was already consumed by the callback. Deleted on successful
// bind or terminal failure; TTL reaps abandoned forms.
type PairConfirm struct {
	Token        string `dynamodbav:"-"`
	Nonce        string `dynamodbav:"nonce"`
	AmazonUserID string `dynamodbav:"amazonUserId"`
	Email        string `dynamodbav:"email,omitempty"`
	Name         string `dynamodbav:"name,omitempty"`
	CreatedAt    int64  `dynamodbav:"createdAt"` // unix seconds
	TTL          int64  `dynamodbav:"ttl"`       // unix seconds
}

// AppHandoff is the one-shot APPHANDOFF#<code>/HANDOFF item (~2-minute TTL)
// bridging the Android broker sign-in: after the shared LWA callback
// resolves + authorizes the identity, it stores the authorized userId here
// keyed by a random code and PKCE-bound to the app instance's
// code_challenge, then 302s the code back to the app via its custom scheme.
// The app claims it (POST /auth/lwa/app-claim) with the matching
// code_verifier to receive its real session — so tokens never travel
// through the custom-scheme URL, and an app that merely intercepted the
// redirect (without the verifier) cannot claim it.
type AppHandoff struct {
	Code         string `dynamodbav:"-"`
	UserID       string `dynamodbav:"userId"`
	AppChallenge string `dynamodbav:"appChallenge"`
	CreatedAt    int64  `dynamodbav:"createdAt"` // unix seconds
	TTL          int64  `dynamodbav:"ttl"`       // unix seconds
}

// Device is the DEVICE#<deviceId>/META item. GSI2: DEVSEEN / <lastSeen
// RFC3339> — the recently-seen feed (a bounded owner+allowlist fleet, so a
// GSI2 Query + userId filter serves per-user listing without a Scan).
type Device struct {
	DeviceID  string `dynamodbav:"deviceId"`
	UserID    string `dynamodbav:"userId"`
	Name      string `dynamodbav:"name"`
	ThingName string `dynamodbav:"thingName,omitempty"`
	CertArn   string `dynamodbav:"certArn,omitempty"`
	CertID    string `dynamodbav:"certId,omitempty"`
	Status    string `dynamodbav:"status"` // active | revoked
	FamilyID  string `dynamodbav:"familyId"`
	CreatedAt int64  `dynamodbav:"createdAt"` // unix seconds
}

// ---- key builders (single source of truth for key formats) ----

func userPK(userID string) string          { return "USER#" + userID }
func lwaGSI1PK(amazonUserID string) string { return "LWA#" + amazonUserID }
func sessSK(sessionID string) string       { return "SESS#" + sessionID }
func sessGSI1PK(sessionID string) string   { return "SESS#" + sessionID }
func sessGSI2PK(userID string) string      { return "USER#" + userID + "#SESS" }
func oauthPK(state string) string          { return "OAUTH#" + state }
func appHandoffPK(code string) string      { return "APPHANDOFF#" + code }
func pairPK(nonce string) string           { return "PAIR#" + nonce }
func pairConfirmPK(token string) string    { return "PAIRCONFIRM#" + token }
func devicePK(deviceID string) string      { return "DEVICE#" + deviceID }
func allowSK(key string) string            { return "ALLOW#" + key }
func usageSK(period string) string         { return "USAGE#" + period }
func noteSK(noteID string) string          { return "NOTE#" + noteID }

const (
	pkConfig      = "CONFIG"
	skOwner       = "OWNER"
	skProfile     = "PROFILE"
	skMeta        = "META"
	skState       = "STATE"
	skHandoff     = "HANDOFF"
	skPair        = "PAIR"
	skConfirm     = "CONFIRM"
	skBucketMint  = "BUCKET#mint"
	gsi2pkDevSeen = "DEVSEEN"
)
