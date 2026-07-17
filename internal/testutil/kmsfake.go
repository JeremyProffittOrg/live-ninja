package testutil

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// FakeKMS implements the Sign/GetPublicKey subset of the KMS client that
// auth.Signer depends on, backed by a locally generated P-256 key — so
// tests exercise the real ES256 sign/verify path with zero AWS calls.
type FakeKMS struct {
	Key *ecdsa.PrivateKey
}

// NewFakeKMS generates a fresh P-256 keypair.
func NewFakeKMS() (*FakeKMS, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("testutil: generate ecdsa key: %w", err)
	}
	return &FakeKMS{Key: key}, nil
}

// Sign mimics kms:Sign for ECDSA_SHA_256 over a RAW message: hash the
// message with SHA-256 and return the ASN.1/DER signature, exactly the
// wire shape the real KMS returns.
func (f *FakeKMS) Sign(ctx context.Context, params *kms.SignInput, optFns ...func(*kms.Options)) (*kms.SignOutput, error) {
	digest := sha256.Sum256(params.Message)
	sig, err := ecdsa.SignASN1(rand.Reader, f.Key, digest[:])
	if err != nil {
		return nil, fmt.Errorf("testutil: ecdsa sign: %w", err)
	}
	return &kms.SignOutput{Signature: sig}, nil
}

// GetPublicKey mimics kms:GetPublicKey: DER SubjectPublicKeyInfo.
func (f *FakeKMS) GetPublicKey(ctx context.Context, params *kms.GetPublicKeyInput, optFns ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
	der, err := x509.MarshalPKIXPublicKey(&f.Key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("testutil: marshal public key: %w", err)
	}
	return &kms.GetPublicKeyOutput{PublicKey: der}, nil
}
