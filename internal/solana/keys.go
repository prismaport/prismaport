// Package solana holds the Solana primitives the backend needs without an SDK: ed25519 keys,
// legacy transaction messages (compile, serialize, sign, decode), and the instructions of the
// programs we call (System, Compute Budget, SPL Token, Associated Token, and the
// prismaport_forwarder program, encoded from its Anchor IDL).
package solana

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/solanaderive"
)

// PublicKey is a 32-byte Solana address.
type PublicKey = solanaderive.PublicKey

// Parsers of base58 public keys; MustPublicKey panics on bad input.
var (
	ParsePublicKey = solanaderive.Parse
	MustPublicKey  = solanaderive.MustParse
)

// ErrKey is returned when a key or seed is malformed.
var ErrKey = errors.New("solana: invalid key")

// Keypair is an ed25519 signing key. It never prints its secret: String, GoString and every fmt
// verb show only the public key.
type Keypair struct {
	priv ed25519.PrivateKey
	pub  PublicKey
}

// KeypairFromSeed returns the keypair of a 32-byte ed25519 seed.
func KeypairFromSeed(seed []byte) (*Keypair, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: seed must be %d bytes", ErrKey, ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	var pub PublicKey
	copy(pub[:], priv[32:])
	return &Keypair{priv: priv, pub: pub}, nil
}

// ParseKeypair accepts the solana-keygen JSON array of 64 bytes (seed ++ public key) or the
// same 64 bytes in base58. The embedded public key must match the seed.
func ParseKeypair(s string) (*Keypair, error) {
	s = strings.TrimSpace(s)
	var raw []byte
	if strings.HasPrefix(s, "[") {
		var arr []int
		if err := json.Unmarshal([]byte(s), &arr); err != nil {
			return nil, fmt.Errorf("%w: not a JSON byte array", ErrKey)
		}
		for _, v := range arr {
			if v < 0 || v > 255 {
				return nil, fmt.Errorf("%w: byte out of range", ErrKey)
			}
			raw = append(raw, byte(v))
		}
	} else {
		b, err := base58.Decode(s)
		if err != nil {
			return nil, fmt.Errorf("%w: not base58", ErrKey)
		}
		raw = b
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: want %d bytes, got %d", ErrKey, ed25519.PrivateKeySize, len(raw))
	}
	k, err := KeypairFromSeed(raw[:32])
	if err != nil {
		return nil, err
	}
	if PublicKey(raw[32:]) != k.pub {
		return nil, fmt.Errorf("%w: public key does not match the seed", ErrKey)
	}
	return k, nil
}

// LoadKeypair reads the key from file when set, else from envValue.
func LoadKeypair(file, envValue string) (*Keypair, error) {
	raw := envValue
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("solana: read key file: %w", err)
		}
		raw = string(b)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%w: none configured", ErrKey)
	}
	return ParseKeypair(raw)
}

// PublicKey returns the keypair's public key.
func (k *Keypair) PublicKey() PublicKey { return k.pub }

// Sign returns the ed25519 signature of msg.
func (k *Keypair) Sign(msg []byte) [64]byte {
	var out [64]byte
	copy(out[:], ed25519.Sign(k.priv, msg))
	return out
}

// JSON renders the solana-keygen file format. Tests only: production keys are never written.
func (k *Keypair) JSON() string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range k.priv {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprint(&b, int(v))
	}
	b.WriteByte(']')
	return b.String()
}

func (k Keypair) String() string { return "solana.Keypair(" + k.pub.String() + ")" }

// GoString shows only the public key, so %#v never prints the secret.
func (k Keypair) GoString() string { return k.String() }

// Format prints only the public key for every fmt verb.
func (k Keypair) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(k.String())) }

// Verify reports whether sig is pub's valid ed25519 signature of msg.
func Verify(pub PublicKey, msg []byte, sig [64]byte) bool {
	return ed25519.Verify(pub[:], msg, sig[:])
}
