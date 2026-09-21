package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"
)

// SignIoTToken returns the base64 (standard encoding, not URL-safe) RSA
// SHA-256 signature AWS IoT expects in x-amz-customauthorizer-signature.
// openssl dgst -sha256 -sign is RSASSA-PKCS1-v1_5, which is what this
// produces. The error text never includes the key or the token.
func SignIoTToken(privateKeyPEM, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", errors.New("iot sign: empty token")
	}
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return "", errors.New("iot sign: private key is not pem")
	}
	key, err := parseIoTPrivateKey(block)
	if err != nil {
		return "", err
	}
	if key.N.BitLen() < 2048 {
		return "", errors.New("iot sign: private key is shorter than 2048 bits")
	}
	sum := sha256.Sum256([]byte(token))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("iot sign: sign failed")
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

func parseIoTPrivateKey(block *pem.Block) (*rsa.PrivateKey, error) {
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("iot sign: private key rejected")
		}
		return key, nil
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("iot sign: private key rejected")
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("iot sign: private key is not rsa")
		}
		return key, nil
	default:
		return nil, errors.New("iot sign: private key type rejected")
	}
}
