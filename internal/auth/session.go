// Package auth implements Live Ninja's first-party session machinery:
// Login-with-Amazon integration, KMS-backed ES256 access JWTs, the JWKS
// document those JWTs verify against, rotating opaque refresh tokens, and
// device pairing. This file owns the signing side — the Signer that mints
// 15-minute ES256 access JWTs via kms:Sign (the private key never leaves
// KMS). Verification (VerifyJWT) and JWKS publication live in jwks.go;
// refresh-token generation lives in refresh.go.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// Token constants fixed by the shared spec (plan.md M1 / contracts/api.md).
const (
	// Issuer is the iss claim on every access JWT.
	Issuer = "https://live.jeremy.ninja"
	// Audience is the aud claim on every access JWT.
	Audience = "live-ninja"
	// AccessTokenTTL is the lifetime of a minted access JWT.
	AccessTokenTTL = 15 * time.Minute
)

// Claims is the payload of a Live Ninja access JWT. Field order matches
// the canonical claim set from the spec: iss/sub/aud/sid/did/surface/
// scope/iat/exp/jti.
type Claims struct {
	Iss     string `json:"iss"`
	Sub     string `json:"sub"` // userId
	Aud     string `json:"aud"`
	Sid     string `json:"sid"`           // sessionId
	Did     string `json:"did,omitempty"` // deviceId (device surface only)
	Surface string `json:"surface"`       // web | android | device
	Scope   string `json:"scope,omitempty"`
	Iat     int64  `json:"iat"`
	Exp     int64  `json:"exp"`
	Jti     string `json:"jti"`
}

// jwtHeader is the fixed JOSE header for every token this package mints.
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// kmsAPI is the subset of the KMS client the Signer depends on, so tests
// can inject a local ECDSA key instead of a real KMS key.
type kmsAPI interface {
	Sign(ctx context.Context, params *kms.SignInput, optFns ...func(*kms.Options)) (*kms.SignOutput, error)
	GetPublicKey(ctx context.Context, params *kms.GetPublicKeyInput, optFns ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error)
}

// Signer mints ES256 access JWTs with an asymmetric KMS key
// (ECC_NIST_P256, SIGN_VERIFY). The private key never leaves KMS; the
// public half is published as a JWKS document (see jwks.go). Safe for
// concurrent use.
type Signer struct {
	client kmsAPI
	keyARN string
	kid    string

	// 24h JWKS cache (jwks.go).
	jwksMu        sync.Mutex
	jwksJSON      []byte
	jwksExpiresAt time.Time
}

// NewSigner builds a Signer for the given KMS key ARN (callers pass the
// JWT_KMS_KEY_ID env value — a full key ARN, per plan.md M0's "key ARNs
// not alias ARNs" decision). AWS credentials come from the ambient Lambda
// execution role.
func NewSigner(ctx context.Context, kmsKeyARN string) (*Signer, error) {
	if kmsKeyARN == "" {
		return nil, errors.New("auth: kms key ARN is required (JWT_KMS_KEY_ID)")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: load aws config: %w", err)
	}
	return &Signer{
		client: kms.NewFromConfig(cfg),
		keyARN: kmsKeyARN,
		kid:    keyIDSuffix(kmsKeyARN),
	}, nil
}

// NewSignerWithClient builds a Signer around an injected KMS client (or a
// test fake implementing Sign/GetPublicKey).
func NewSignerWithClient(client kmsAPI, kmsKeyARN string) *Signer {
	return &Signer{
		client: client,
		keyARN: kmsKeyARN,
		kid:    keyIDSuffix(kmsKeyARN),
	}
}

// KeyID returns the kid this Signer stamps into JWT headers and the JWKS —
// the key-id suffix of the KMS key ARN.
func (s *Signer) KeyID() string { return s.kid }

// SignAccessToken mints an ES256 JWT for the given claims via kms:Sign.
// Zero-valued standard claims are filled with their canonical defaults:
// iss=Issuer, aud=Audience, iat=now, exp=iat+AccessTokenTTL, and a random
// jti — so callers only need to populate Sub/Sid/Surface (+Did/Scope).
func (s *Signer) SignAccessToken(ctx context.Context, c Claims) (string, error) {
	now := time.Now().Unix()
	if c.Iss == "" {
		c.Iss = Issuer
	}
	if c.Aud == "" {
		c.Aud = Audience
	}
	if c.Iat == 0 {
		c.Iat = now
	}
	if c.Exp == 0 {
		c.Exp = c.Iat + int64(AccessTokenTTL/time.Second)
	}
	if c.Jti == "" {
		jti, err := randomToken(16)
		if err != nil {
			return "", fmt.Errorf("auth: generate jti: %w", err)
		}
		c.Jti = jti
	}
	if c.Sub == "" {
		return "", errors.New("auth: claims missing sub (userId)")
	}

	headerJSON, err := json.Marshal(jwtHeader{Alg: "ES256", Typ: "JWT", Kid: s.kid})
	if err != nil {
		return "", fmt.Errorf("auth: marshal jwt header: %w", err)
	}
	claimsJSON, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("auth: marshal jwt claims: %w", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) +
		"." + base64.RawURLEncoding.EncodeToString(claimsJSON)

	out, err := s.client.Sign(ctx, &kms.SignInput{
		KeyId:            &s.keyARN,
		Message:          []byte(signingInput),
		MessageType:      kmstypes.MessageTypeRaw,
		SigningAlgorithm: kmstypes.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		return "", fmt.Errorf("auth: kms sign: %w", err)
	}

	rawSig, err := derSigToRaw(out.Signature)
	if err != nil {
		return "", fmt.Errorf("auth: convert kms signature: %w", err)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(rawSig), nil
}

// ecdsaDERSig mirrors the ASN.1 SEQUENCE { r INTEGER, s INTEGER } that
// KMS returns for ECDSA signatures.
type ecdsaDERSig struct {
	R, S *big.Int
}

// derSigToRaw converts a DER-encoded ECDSA signature (as returned by
// kms:Sign) into the fixed-width 64-byte r||s form that JOSE ES256
// requires (RFC 7518 §3.4): each integer left-padded to exactly 32 bytes.
func derSigToRaw(der []byte) ([]byte, error) {
	var sig ecdsaDERSig
	rest, err := asn1.Unmarshal(der, &sig)
	if err != nil {
		return nil, fmt.Errorf("parse der signature: %w", err)
	}
	if len(rest) != 0 {
		return nil, errors.New("trailing bytes after der signature")
	}
	if sig.R == nil || sig.S == nil || sig.R.Sign() <= 0 || sig.S.Sign() <= 0 {
		return nil, errors.New("der signature has non-positive integers")
	}
	if sig.R.BitLen() > 256 || sig.S.BitLen() > 256 {
		return nil, errors.New("der signature integers exceed 256 bits")
	}
	raw := make([]byte, 64)
	sig.R.FillBytes(raw[:32])
	sig.S.FillBytes(raw[32:])
	return raw, nil
}

// keyIDSuffix derives the JWT kid from a KMS key ARN: the segment after
// the final "/" (the key UUID). A bare key id passes through unchanged.
func keyIDSuffix(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

// randomToken returns n cryptographically-random bytes, base64url
// (unpadded) encoded.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
