package codeupdate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Run token
// ---------------------------------------------------------------------------

func TestTokenRoundTrip(t *testing.T) {
	const reqID = "019fa317-aaeb-715d-b80b-c2f87e129228"
	token, hash, err := NewToken(reqID)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if !strings.HasPrefix(token, TokenPrefix) {
		t.Errorf("token %q lacks the %q prefix that makes it recognisable on sight", token, TokenPrefix)
	}
	gotID, secret, err := ParseToken(token)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if gotID != reqID {
		t.Errorf("request id = %q, want %q", gotID, reqID)
	}
	if len(secret) != TokenSecretLen {
		t.Errorf("secret is %d chars, want %d", len(secret), TokenSecretLen)
	}
	if err := VerifySecret(secret, hash); err != nil {
		t.Errorf("VerifySecret rejected the secret it just minted: %v", err)
	}
}

// The stored value must never be the secret itself: a leaked table, backup or
// log line has to yield nothing usable.
func TestStoredHashIsNotTheSecret(t *testing.T) {
	token, hash, err := NewToken("req-1")
	if err != nil {
		t.Fatal(err)
	}
	_, secret, _ := ParseToken(token)
	if hash == secret {
		t.Fatal("the stored hash IS the secret")
	}
	if strings.Contains(hash, secret) || strings.Contains(secret, hash) {
		t.Fatal("the stored hash contains the secret")
	}
}

func TestTokensAreUnique(t *testing.T) {
	seen := make(map[string]bool, 200)
	for range 200 {
		token, _, err := NewToken("req-1")
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatal("NewToken produced a duplicate")
		}
		seen[token] = true
	}
}

// ParseToken is the shape gate that keeps a session cookie or a bearer JWT from
// ever reaching the secret comparison.
func TestParseTokenRejectsForeignCredentials(t *testing.T) {
	valid, _, _ := NewToken("req-1")
	_, secret, _ := ParseToken(valid)

	for _, bad := range []string{
		"",
		"   ",
		"gk_" + strings.Repeat("a", 32) + "_" + secret, // a ghost-cli API key
		"eyJhbGciOiJSUzI1NiJ9.body.sig",                // a JWT
		"cu_",
		"cu_req-1",                            // no secret
		"cu_req-1_",                           // empty secret
		"cu_req-1_" + strings.Repeat("a", 63), // secret too short
		"cu_req-1_" + strings.Repeat("a", 65), // secret too long
		"cu_req-1_" + strings.Repeat("Z", 64), // not hex
		"cu__" + secret,                       // empty request id
		"CU_req-1_" + secret,                  // wrong case prefix
	} {
		if _, _, err := ParseToken(bad); err == nil {
			t.Errorf("ParseToken(%q) accepted a credential it must refuse", bad)
		}
	}
}

func TestVerifySecretRejectsWrongAndEmpty(t *testing.T) {
	_, hash, _ := NewToken("req-1")
	if err := VerifySecret(strings.Repeat("0", TokenSecretLen), hash); err == nil {
		t.Error("VerifySecret accepted a wrong secret")
	}
	if err := VerifySecret("anything", ""); err == nil {
		t.Error("VerifySecret accepted an empty stored hash — an unset row must never authenticate")
	}
}

// The token is parsed by splitting on "_", so an id containing one would make
// the split ambiguous. Refuse at mint rather than issue an unparseable token.
func TestNewTokenRejectsAmbiguousRequestIDs(t *testing.T) {
	for _, bad := range []string{"", "   ", "has_underscore"} {
		if _, _, err := NewToken(bad); err == nil {
			t.Errorf("NewToken(%q) succeeded; it must refuse", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Event id
// ---------------------------------------------------------------------------

// The event id must be STABLE per repo — that is what makes repeat updates
// accumulate under one cockpit event and what arms ghost-cli's in-flight guard.
func TestEventIDIsStableAndSafe(t *testing.T) {
	cases := map[string]string{
		"JeremyProffittOrg/live-ninja": "voice-update-jeremyproffittorg-live-ninja",
		"o/ghost-cli":                  "voice-update-o-ghost-cli",
		"O/Ghost.CLI":                  "voice-update-o-ghost-cli",
	}
	for repo, want := range cases {
		if got := EventID(repo); got != want {
			t.Errorf("EventID(%q) = %q, want %q", repo, got, want)
		}
	}
	if EventID("o/repo") != EventID("o/repo") {
		t.Error("EventID is not stable across calls")
	}
	if EventID("o/a") == EventID("o/b") {
		t.Error("EventID collides across different repos")
	}
	// Whatever the repo string contains, the result must stay a plain slug: it
	// becomes a DynamoDB key and appears in audit reasons.
	for _, nasty := range []string{"o/../../etc", "o/re po", "o/re\npo", "o/ペ"} {
		got := EventID(nasty)
		for _, r := range got {
			isOK := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
			if !isOK {
				t.Errorf("EventID(%q) = %q contains unsafe rune %q", nasty, got, r)
			}
		}
	}
}

func TestValidCLI(t *testing.T) {
	for _, good := range SupportedCLIs {
		if !ValidCLI(good) {
			t.Errorf("ValidCLI(%q) = false", good)
		}
	}
	// ghost-cli accepts more agents than this voice surface offers; the extra
	// ones must not leak through.
	for _, bad := range []string{"", "grok", "opencode", "antigravity", "CLAUDE", "claude "} {
		if ValidCLI(bad) {
			t.Errorf("ValidCLI(%q) = true", bad)
		}
	}
}

// DynamoDB's TTL sweep is best-effort and routinely runs hours behind, so an
// expired row stays readable. A token whose run ended yesterday must not keep
// working just because a background deleter is late.
func TestExpiredTokenRowReadsAsAbsent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	db := newFakeDDB()
	s := NewStore(db, "live-ninja", func() time.Time { return now })

	if err := s.PutToken(context.Background(), TokenRow{
		RequestID: "req-1", UserID: "u1", Repo: "o/r", TokenHash: "hash",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetToken(context.Background(), "req-1"); err != nil {
		t.Fatalf("fresh row should read: %v", err)
	}

	now = now.Add(RecordTTL + time.Second)
	if _, err := s.GetToken(context.Background(), "req-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired row returned %v, want ErrNotFound so the caller's uniform 401 covers it", err)
	}
}

// PutToken is create-only: a second mint for the same request must be refused
// rather than silently revoking the first.
func TestPutTokenIsCreateOnly(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewStore(newFakeDDB(), "live-ninja", func() time.Time { return now })
	row := TokenRow{RequestID: "req-1", UserID: "u1", Repo: "o/r", TokenHash: "hash-1"}

	if err := s.PutToken(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	row.TokenHash = "hash-2"
	if err := s.PutToken(context.Background(), row); !errors.Is(err, ErrTokenExists) {
		t.Fatalf("second mint returned %v, want ErrTokenExists", err)
	}
	got, err := s.GetToken(context.Background(), "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TokenHash != "hash-1" {
		t.Errorf("token hash = %q; the refused mint overwrote the original", got.TokenHash)
	}
}

// The node name is an EXACT IoT Thing name. ghost-cli's ACL compares it
// byte-for-byte with no case folding, and it is interpolated into the command
// topic (cockpit/nodes/<name>/cmd) — so a case slip either fails the capability
// check or publishes to a topic nothing subscribes to, leaving the run wedged
// RUNNING until a 2h grace marks it FAILED. Neither failure names the cause.
//
// Verified against the live fleet on 2026-07-30: `aws iot list-things` lists
// OFFICEPC. If the node is ever renamed, this constant moves with it.
func TestDefaultNodeIsTheExactThingName(t *testing.T) {
	if DefaultNode != "OFFICEPC" {
		t.Fatalf("DefaultNode = %q, want the exact IoT Thing name %q", DefaultNode, "OFFICEPC")
	}
	if strings.ToLower(DefaultNode) == DefaultNode {
		t.Error("DefaultNode was lower-cased; ghost-cli's node ACL does not case-fold")
	}
}
