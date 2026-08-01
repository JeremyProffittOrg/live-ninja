// Package codeupdate is the shared spine of the voice-driven code-update
// feature: the queue message the `code_update_start` tool enqueues, the
// CODEUPD# row that tracks a request end to end, and the per-run token the
// coding agent uses to email progress back.
//
// Three components meet here and must agree exactly:
//
//	internal/tools/codeupdate.go   enqueues a Request, reads a Record
//	cmd/codeupdate-dispatch        consumes a Request, owns the Record
//	internal/webapp/codeupdate_*   verifies a token against the Record
//
// so the shapes live in one place rather than being restated three times.
package codeupdate

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// QueueMessageVersion is stamped on every Request. The worker refuses a version
// it does not know rather than guessing at a field it cannot see — an in-flight
// message during a deploy is the case this exists for.
const QueueMessageVersion = 1

// Defaults. Node and CLI are the answers to "update an application" when the
// owner does not say where or with what.
//
// DefaultNode is the node's EXACT IoT Thing name, uppercase and all. Two
// independent things match it byte-for-byte and neither tells you when you are
// wrong: ghost-cli's node ACL (authz.Entry.AllowsNode is an exact compare, no
// case folding) and the command topic the envelope is published to
// (cockpit/nodes/<name>/cmd). A lowercase "officepc" therefore fails the
// capability check, or — if the principal held a wildcard — publishes to a topic
// nothing is subscribed to, and the run sits RUNNING until the 2h grace marks it
// FAILED. Verified against the live fleet: `aws iot list-things` lists OFFICEPC.
const (
	DefaultNode       = "OFFICEPC"
	DefaultCLI        = "claude"
	DefaultOutputFile = "update-report.md"
)

// SupportedCLIs is the closed set this feature exposes by voice. ghost-cli
// itself accepts more (grok, opencode, antigravity), but these two are the ones
// the owner asked for, and a voice surface should not offer a choice nobody
// makes out loud.
var SupportedCLIs = []string{"claude", "codex"}

// ValidCLI reports whether cli is one this feature will launch.
func ValidCLI(cli string) bool { return slices.Contains(SupportedCLIs, cli) }

// Request is the SQS message body `code_update_start` writes. It carries what
// the owner asked for and nothing derived: the worker owns the prompt, the
// token and the launch.
type Request struct {
	Version   int    `json:"version"`
	RequestID string `json:"requestId"`
	UserID    string `json:"userId"`
	SessionID string `json:"sessionId,omitempty"`

	Repo         string `json:"repo"`
	Instructions string `json:"instructions"`
	Node         string `json:"node"`
	CLI          string `json:"cli"`
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`

	// Preprocess asks for the Opus rewrite. Default true; false only when the
	// owner said not to.
	Preprocess bool `json:"preprocess"`
	// Deploy lets the session push to main, which is the production deploy
	// trigger in these repos. Default TRUE as of the owner's 2026-08-01
	// decision: work the owner already confirmed is expected to ship, and the
	// closed default was stranding finished changes unpushed on a machine
	// nobody was watching. The owner opts OUT explicitly ("...but don't push").
	//
	// The wire default is still the zero value, and that is deliberate: a
	// malformed or truncated queue message decodes to false and holds, rather
	// than deploying on the strength of a field that never arrived. Only the
	// tool boundary, where real owner intent is known, flips it on.
	Deploy bool `json:"deploy"`

	// RequestedAt is when the tool accepted the request, RFC3339. Used for the
	// confirmation email and for the row's created timestamp, so the worker
	// never has to guess how long a message sat in the queue.
	RequestedAt string `json:"requestedAt"`
}

// Request statuses, in the order a healthy request moves through them.
const (
	StatusQueued        = "queued"
	StatusPreprocessing = "preprocessing"
	StatusLaunching     = "launching"
	StatusLaunched      = "launched"
	StatusFailed        = "failed"
)

// Record is the CODEUPD# item. It is the single source of truth for "what
// happened to that update I asked for", read by `code_update_status` and by the
// progress endpoint's token check.
type Record struct {
	RequestID string `json:"requestId"`
	UserID    string `json:"userId"`
	Status    string `json:"status"`

	Repo   string `json:"repo"`
	Node   string `json:"node"`
	CLI    string `json:"cli"`
	Model  string `json:"model,omitempty"`
	Deploy bool   `json:"deploy"`

	// Instructions is what the owner actually asked for, in their own words.
	//
	// Owner decision, 2026-07-31: this row is the only durable copy. Everything
	// else that carries the words is transient — the SQS message is consumed,
	// and the launch email is outside this system. The first incident on this
	// feature was nearly unrecoverable for exactly that reason: the request had
	// failed and nothing left in the account could say what had been asked for.
	//
	// The privacy cost is real and is bounded deliberately: the row already
	// carries RecordTTL, so the words live 24 hours and no longer, they stay in
	// the owner's own partition behind the same per-call authorization as the
	// rest of the record, and they are never logged. They are also deliberately
	// NOT added to the code_update_status response — the reader here is a human
	// diagnosing a failed run, not the model.
	Instructions string `json:"instructions,omitempty"`

	// Rewritten records whether the launched prompt was the Opus rewrite or the
	// owner's own words. It is reported, never silent: a rewrite that failed is
	// something the owner should know about their own instructions.
	Rewritten bool `json:"rewritten"`
	// RewriteNote explains a false Rewritten (timed out, failed, not requested).
	RewriteNote string `json:"rewriteNote,omitempty"`

	EventID string `json:"eventId,omitempty"`
	RunID   string `json:"runId,omitempty"`
	Error   string `json:"error,omitempty"`

	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// SortKey is the CODEUPD# sort key for a request. The user partition holds it,
// so a request is only ever readable by the account that asked for it.
func SortKey(requestID string) string { return "CODEUPD#" + requestID }

// MaxProgressPosts bounds how many progress emails one run may send. The prompt
// asks for three; eight leaves room for a chattier session without turning a
// looping agent into an inbox flood.
const MaxProgressPosts = 8

// RecordTTL is how long a CODEUPD# row (and therefore its token) lives. A run
// that has not finished inside a day is not going to, and the token should not
// outlive the work it was minted for.
const RecordTTL = 24 * time.Hour

// ---------------------------------------------------------------------------
// Run token
// ---------------------------------------------------------------------------

// The run-token wire format, deliberately modelled on ghost-cli's `gk_` API
// keys:
//
//	cu_<requestId>_<secret>
//
// The prefix makes it recognisable on sight — in a log, a shell history, a
// committed file — and, more usefully, recognisable BEFORE any store is
// touched, so a session cookie or a bearer JWT can never reach this validator.
// Only a hash of the secret is ever stored, so the row yields nothing usable.
const (
	// TokenPrefix is the visible namespace of a code-update run token.
	TokenPrefix = "cu_"
	// TokenSecretLen is the secret's length in lowercase hex (32 random bytes =
	// 256 bits — not brute-forceable, so a single SHA-256 is the right hash and
	// no password-style KDF is warranted).
	TokenSecretLen = 64
)

var (
	// ErrTokenMalformed means the credential is not token-shaped. Callers MUST
	// NOT distinguish this from a wrong secret in any response: the two together
	// are one 401.
	ErrTokenMalformed = errors.New("codeupdate: malformed run token")
	// ErrTokenMismatch means the secret did not match the stored hash.
	ErrTokenMismatch = errors.New("codeupdate: run token does not match")
)

// NewToken mints a run token for requestID and returns the plaintext token and
// the hash to store. The plaintext must be written into the prompt and then
// forgotten.
func NewToken(requestID string) (token, hash string, err error) {
	if strings.TrimSpace(requestID) == "" {
		return "", "", errors.New("codeupdate: request id is required to mint a token")
	}
	if strings.Contains(requestID, "_") {
		// The token is parsed by splitting on "_", so an id containing one would
		// make the split ambiguous. Request ids are UUIDv7 and never contain one;
		// this refuses rather than minting a token that cannot be parsed back.
		return "", "", errors.New("codeupdate: request id must not contain an underscore")
	}
	buf := make([]byte, TokenSecretLen/2)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("codeupdate: mint token: %w", err)
	}
	secret := hex.EncodeToString(buf)
	return TokenPrefix + requestID + "_" + secret, HashSecret(secret), nil
}

// HashSecret hashes a token secret for storage/comparison.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ParseToken splits a run token into its request id and secret. It validates
// SHAPE only — whether the secret is right is VerifySecret's job, against a
// row this function knows nothing about.
func ParseToken(token string) (requestID, secret string, err error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, TokenPrefix) {
		return "", "", ErrTokenMalformed
	}
	rest := token[len(TokenPrefix):]
	requestID, secret, ok := strings.Cut(rest, "_")
	if !ok || requestID == "" || len(secret) != TokenSecretLen {
		return "", "", ErrTokenMalformed
	}
	if !isHex(secret) {
		return "", "", ErrTokenMalformed
	}
	return requestID, secret, nil
}

// VerifySecret compares a presented secret against a stored hash in constant
// time.
func VerifySecret(secret, storedHash string) error {
	if storedHash == "" {
		return ErrTokenMismatch
	}
	got := HashSecret(secret)
	if subtle.ConstantTimeCompare([]byte(got), []byte(storedHash)) != 1 {
		return ErrTokenMismatch
	}
	return nil
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Event id
// ---------------------------------------------------------------------------

// EventID derives the stable ghost-cli schedule event id for a repo. Reusing it
// across dispatches is deliberate: repeated "update live-ninja" requests then
// accumulate under ONE cockpit event with a run history and a Run-now button,
// instead of littering the schedule list with a new row per sentence.
//
// It is also what makes ghost-cli's in-flight guard bite — a second update to
// the same repo while the first is still running is refused rather than stacking
// two sessions on one machine.
func EventID(repo string) string {
	var b strings.Builder
	b.WriteString("voice-update-")
	prev := byte('-')
	for i := 0; i < len(repo); i++ {
		c := repo[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 32)
			prev = c + 32
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
			prev = c
		default:
			if prev != '-' {
				b.WriteByte('-')
				prev = '-'
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}
