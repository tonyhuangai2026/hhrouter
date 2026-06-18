// Package crypto provides symmetric encryption helpers for protecting secrets
// at rest (Tech Design §11). Channel upstream keys are stored AES-GCM encrypted
// using key material derived from the SECRET_KEY environment variable.
//
// The helpers here are intentionally dependency-free so downstream tasks (e.g.
// T6 upstream adapters) can decrypt a channel key with the same secret.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// ErrEmptyCiphertext is returned when Decrypt is given an empty input.
var ErrEmptyCiphertext = errors.New("crypto: empty ciphertext")

// deriveKey turns an arbitrary-length secret into a fixed 32-byte AES-256 key
// via SHA-256. This lets operators use any SECRET_KEY string while still
// feeding a valid key length to AES.
func deriveKey(secret string) [32]byte {
	return sha256.Sum256([]byte(secret))
}

// Encrypt AES-256-GCM encrypts plaintext with a key derived from secret and
// returns a base64 (std) encoded string of nonce||ciphertext||tag. An empty
// plaintext encrypts to an empty string so that channels without a key stay
// empty rather than storing an encrypted empty value.
func Encrypt(secret, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	key := deriveKey(secret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: new gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: read nonce: %w", err)
	}
	// Seal appends the ciphertext (and tag) to nonce, so the returned blob is
	// nonce || ciphertext || tag.
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt: it base64-decodes the blob, splits off the nonce
// and AES-256-GCM decrypts the remainder using a key derived from secret. An
// empty ciphertext decrypts to an empty string.
func Decrypt(secret, ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("crypto: base64 decode: %w", err)
	}
	key := deriveKey(secret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: new gcm: %w", err)
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return "", errors.New("crypto: ciphertext shorter than nonce")
	}
	nonce, body := raw[:ns], raw[ns:]
	plain, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: gcm open: %w", err)
	}
	return string(plain), nil
}

// Mask returns a display-safe representation of a secret, revealing only a few
// leading and trailing characters (e.g. "sk-1***wxyz"). Short secrets are fully
// masked. The empty string maps to the empty string.
func Mask(secret string) string {
	if secret == "" {
		return ""
	}
	const head, tail = 4, 4
	if len(secret) <= head+tail {
		return "****"
	}
	return secret[:head] + "****" + secret[len(secret)-tail:]
}
