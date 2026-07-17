// JWKS publication and pure-Go JWT verification for Live Ninja access
// tokens. The Signer (session.go) mints ES256 JWTs via kms:Sign; this
// file exposes the matching public key as a JWKS document (served at
// /.well-known/jwks.json) and a verifier that needs only that JSON — no
// AWS calls — so the Lambda authorizer, the web function's local-dev
// fallback, and tests all verify tokens the exact same way.
package auth

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// jwksCacheTTL is how long a built JWKS document is served from memory
// before Signer.JWKS re-fetches the public key from KMS. The key is
// static for the stack's lifetime, so 24h simply bounds staleness after
// a (rare, manual) key rotation.
const jwksCacheTTL = 24 * time.Hour

// Sentinel errors for verification failures. ErrTokenExpired is split
// out so the authorizer can distinguish "expired, go refresh" from
// "malformed/forged" without string matching.
var (
	ErrInvalidToken = errors.New("auth: invalid token")
	ErrTokenExpired = errors.New("auth: token expired")
)

// clockSkew is the leeway allowed when checking exp/iat, covering minor
// clock drift between the signer Lambda and the verifier Lambda.
const clockSkew = 60 * time.Second

// jwk is a single JSON Web Key — the P-256 public key in RFC 7517/7518
// form. x and y are unpadded base64url of the fixed 32-byte big-endian
// coordinates.
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

type jwksDoc struct {
	Keys []jwk `json:"keys"`
}

// JWKS returns the JWKS JSON document for this Signer's KMS key, built
// from kms:GetPublicKey (DER SubjectPublicKeyInfo → EC point → JWK) and
// cached in memory for jwksCacheTTL. The returned slice is shared with
// the cache — callers must not mutate it.
func (s *Signer) JWKS(ctx context.Context) ([]byte, error) {
	s.jwksMu.Lock()
	defer s.jwksMu.Unlock()
	if s.jwksJSON != nil && time.Now().Before(s.jwksExpiresAt) {
		return s.jwksJSON, nil
	}

	out, err := s.client.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: &s.keyARN})
	if err != nil {
		return nil, fmt.Errorf("auth: kms get public key: %w", err)
	}

	parsed, err := x509.ParsePKIXPublicKey(out.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("auth: parse kms public key der: %w", err)
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("auth: kms public key is %T, want *ecdsa.PublicKey", parsed)
	}
	if pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("auth: kms public key curve is %s, want P-256", pub.Curve.Params().Name)
	}

	var xb, yb [32]byte
	pub.X.FillBytes(xb[:])
	pub.Y.FillBytes(yb[:])

	doc := jwksDoc{Keys: []jwk{{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(xb[:]),
		Y:   base64.RawURLEncoding.EncodeToString(yb[:]),
		Kid: s.kid,
		Alg: "ES256",
		Use: "sig",
	}}}
	buf, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("auth: marshal jwks: %w", err)
	}

	s.jwksJSON = buf
	s.jwksExpiresAt = time.Now().Add(jwksCacheTTL)
	return buf, nil
}

// VerifyJWT verifies a Live Ninja access JWT against a JWKS document
// using pure crypto/ecdsa — no network or AWS calls. It checks, in
// order: token structure, header alg (ES256 only — alg:none and
// algorithm-substitution are rejected), key lookup by kid, the ECDSA
// P-256 signature over the signing input, and then the claims:
// iss == Issuer, aud == Audience, exp (with clockSkew leeway; returns
// ErrTokenExpired), and iat not meaningfully in the future. The
// tokensValidAfter kill-switch check is intentionally NOT here — it
// needs a store lookup and belongs to the authorizer (plan.md M1).
func VerifyJWT(token string, jwksJSON []byte) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: expected 3 segments, got %d", ErrInvalidToken, len(parts))
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header not base64url: %v", ErrInvalidToken, err)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return nil, fmt.Errorf("%w: header not json: %v", ErrInvalidToken, err)
	}
	if hdr.Alg != "ES256" {
		return nil, fmt.Errorf("%w: alg %q not allowed (ES256 only)", ErrInvalidToken, hdr.Alg)
	}

	var doc jwksDoc
	if err := json.Unmarshal(jwksJSON, &doc); err != nil {
		return nil, fmt.Errorf("auth: parse jwks: %w", err)
	}
	key, err := selectKey(doc, hdr.Kid)
	if err != nil {
		return nil, err
	}
	pub, err := jwkToECDSA(key)
	if err != nil {
		return nil, err
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature not base64url: %v", ErrInvalidToken, err)
	}
	if len(sig) != 64 {
		return nil, fmt.Errorf("%w: es256 signature must be 64 bytes, got %d", ErrInvalidToken, len(sig))
	}
	r := new(big.Int).SetBytes(sig[:32])
	sVal := new(big.Int).SetBytes(sig[32:])
	if r.Sign() == 0 || sVal.Sign() == 0 {
		return nil, fmt.Errorf("%w: zero signature integer", ErrInvalidToken)
	}

	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], r, sVal) {
		return nil, fmt.Errorf("%w: signature verification failed", ErrInvalidToken)
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: claims not base64url: %v", ErrInvalidToken, err)
	}
	var c Claims
	if err := json.Unmarshal(claimsJSON, &c); err != nil {
		return nil, fmt.Errorf("%w: claims not json: %v", ErrInvalidToken, err)
	}

	now := time.Now()
	if c.Iss != Issuer {
		return nil, fmt.Errorf("%w: iss %q, want %q", ErrInvalidToken, c.Iss, Issuer)
	}
	if c.Aud != Audience {
		return nil, fmt.Errorf("%w: aud %q, want %q", ErrInvalidToken, c.Aud, Audience)
	}
	if c.Exp <= 0 || now.After(time.Unix(c.Exp, 0).Add(clockSkew)) {
		return nil, ErrTokenExpired
	}
	if c.Iat > now.Add(clockSkew).Unix() {
		return nil, fmt.Errorf("%w: iat is in the future", ErrInvalidToken)
	}

	return &c, nil
}

// selectKey picks the JWK matching kid; with an empty kid it accepts a
// single-key set (our steady state) but refuses to guess among several.
func selectKey(doc jwksDoc, kid string) (jwk, error) {
	if kid == "" {
		if len(doc.Keys) == 1 {
			return doc.Keys[0], nil
		}
		return jwk{}, fmt.Errorf("%w: no kid and %d keys in jwks", ErrInvalidToken, len(doc.Keys))
	}
	for _, k := range doc.Keys {
		if k.Kid == kid {
			return k, nil
		}
	}
	return jwk{}, fmt.Errorf("%w: kid %q not found in jwks", ErrInvalidToken, kid)
}

// jwkToECDSA reconstructs a P-256 public key from a JWK, validating the
// key type, curve, coordinate width, and — via crypto/ecdh — that the
// point actually lies on the curve (rejecting invalid-point inputs).
func jwkToECDSA(k jwk) (*ecdsa.PublicKey, error) {
	if k.Kty != "EC" || k.Crv != "P-256" {
		return nil, fmt.Errorf("%w: jwk kty/crv %q/%q, want EC/P-256", ErrInvalidToken, k.Kty, k.Crv)
	}
	xb, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, fmt.Errorf("%w: jwk x not base64url: %v", ErrInvalidToken, err)
	}
	yb, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil {
		return nil, fmt.Errorf("%w: jwk y not base64url: %v", ErrInvalidToken, err)
	}
	if len(xb) == 0 || len(xb) > 32 || len(yb) == 0 || len(yb) > 32 {
		return nil, fmt.Errorf("%w: jwk coordinate length out of range", ErrInvalidToken)
	}

	// Fixed-width uncompressed point 0x04 || X || Y for on-curve
	// validation through crypto/ecdh (which rejects off-curve points).
	point := make([]byte, 65)
	point[0] = 4
	copy(point[1+(32-len(xb)):33], xb)
	copy(point[33+(32-len(yb)):], yb)
	if _, err := ecdh.P256().NewPublicKey(point); err != nil {
		return nil, fmt.Errorf("%w: jwk point not on P-256: %v", ErrInvalidToken, err)
	}

	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xb),
		Y:     new(big.Int).SetBytes(yb),
	}, nil
}
