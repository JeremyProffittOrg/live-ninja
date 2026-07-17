// Opaque rotating refresh tokens (plan.md M1, Auth §2.4). The plaintext
// token is returned to the client exactly once (cookie for web, JSON for
// Android/device); only its SHA-256 hex hash is ever stored in DynamoDB
// (SESSION.refreshHash / prevHash). Rotation, sliding-window renewal, and
// reuse-detection live in store.RotateRefresh — this file owns generation
// and the canonical hash so every code path hashes identically.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Session lifetime windows fixed by the shared spec.
const (
	// SlidingWindow is the web/Android refresh lifetime: 30 days,
	// slid forward on every successful rotation.
	SlidingWindow = 30 * 24 * time.Hour
	// DeviceWindow is the M5Stack device credential lineage lifetime:
	// 10 years (10 × 365 days).
	DeviceWindow = 10 * 365 * 24 * time.Hour
)

// GenerateRefreshToken mints a new opaque refresh token: 32
// cryptographically-random bytes, base64url (unpadded) encoded. It
// returns the plaintext (sent to the client, never stored) and its
// SHA-256 hex hash (stored in the SESSION item as refreshHash).
func GenerateRefreshToken() (plaintext string, hash string, err error) {
	plaintext, err = randomToken(32)
	if err != nil {
		return "", "", err
	}
	return plaintext, HashRefreshToken(plaintext), nil
}

// HashRefreshToken returns the canonical stored form of a refresh token:
// lowercase hex SHA-256 of the plaintext token string. Handlers hash the
// presented token with this before comparing/rotating against DynamoDB —
// a raw token never touches the table or the logs.
func HashRefreshToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
