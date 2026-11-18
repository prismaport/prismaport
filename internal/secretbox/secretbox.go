// Package secretbox encrypts small secrets at rest (webhook signing secrets) with AES-256-GCM.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrDecrypt is returned when a sealed value fails to open with the key and associated data.
var ErrDecrypt = errors.New("secretbox: decryption failed")

// Box seals and opens secrets with an authenticated cipher.
type Box struct {
	aead cipher.AEAD
	key  []byte
}

// New takes a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secretbox: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, key: append([]byte(nil), key...)}, nil
}

// DeriveKey returns a 32-byte key for another use (label), so the one configured secret can
// also key MACs without any two uses sharing a key.
func (b *Box) DeriveKey(label string) []byte {
	m := hmac.New(sha256.New, b.key)
	m.Write([]byte("prismaport/secretbox/derive/" + label))
	return m.Sum(nil)
}

// FromHex takes the key as 64 hex characters.
func FromHex(s string) (*Box, error) {
	key, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("secretbox: key is not hex: %w", err)
	}
	return New(key)
}

// Seal returns nonce || ciphertext. aad binds the ciphertext to its owner (e.g. endpoint id)
// so a ciphertext copied to another row does not decrypt.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize(), b.aead.NonceSize()+len(plaintext)+b.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts a value Seal produced with the same associated data, or returns ErrDecrypt.
func (b *Box) Open(sealed, aad []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return nil, ErrDecrypt
	}
	out, err := b.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return out, nil
}
