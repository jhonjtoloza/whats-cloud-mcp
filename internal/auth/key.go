// Package auth owns the security boundary of the gateway: API key generation,
// hashing, scope evaluation and the net/http middleware that turns a bearer
// token into a Principal.
//
// Two credential classes exist and they never overlap:
//
//   - ADMIN: a single shared token read from the environment. It may create
//     tenants, issue and revoke keys and start pairing. It may NOT send
//     messages or read conversations.
//   - TENANT: a per-tenant API key with explicit scopes. It is bound to exactly
//     one tenant and can never reach another tenant's data.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
)

const (
	// keyPrefixLiteral is the fixed leading segment of every issued key.
	keyPrefixLiteral = "wc_live"
	// tenantShortLen is how many alphanumeric characters of the tenant id are
	// embedded in the key so an operator can tell keys apart at a glance.
	tenantShortLen = 8
	// secretBytes is the amount of cryptographically random material behind
	// every key. 32 bytes = 256 bits.
	secretBytes = 32
	// displayedSecretChars is how much of the secret the stored prefix keeps so
	// a key can be identified in a UI without being usable.
	displayedSecretChars = 6
)

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// ErrInvalidTenantID is returned when a tenant id carries no usable characters.
var ErrInvalidTenantID = errors.New("auth: tenant id must contain at least one alphanumeric character")

// GeneratedKey is the result of issuing a new API key. Plaintext is the only
// moment the caller will ever see the secret: only Hash and Prefix are stored.
type GeneratedKey struct {
	// Plaintext is the full key handed to the client exactly once.
	Plaintext string
	// Hash is the lowercase hex SHA-256 digest of Plaintext.
	Hash string
	// Prefix is the non-secret leading portion, safe to display and log.
	Prefix string
}

// GenerateKey issues a new API key for the given tenant. The returned key has
// the shape wc_live_<tenantShort>_<base62 of 32 random bytes>.
func GenerateKey(tenantID string) (GeneratedKey, error) {
	short := shortenTenantID(tenantID)
	if short == "" {
		return GeneratedKey{}, ErrInvalidTenantID
	}

	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return GeneratedKey{}, err
	}

	secret := encodeBase62(raw)
	plaintext := keyPrefixLiteral + "_" + short + "_" + secret

	prefixLen := len(plaintext) - len(secret) + displayedSecretChars
	return GeneratedKey{
		Plaintext: plaintext,
		Hash:      HashKey(plaintext),
		Prefix:    plaintext[:prefixLen],
	}, nil
}

// HashKey returns the lowercase hex SHA-256 digest of a plaintext key.
//
// Deliberately NOT bcrypt/argon2: these keys are 256 bits of output from
// crypto/rand, not human-chosen passwords. A password KDF exists to make
// low-entropy secrets expensive to brute force; against a uniformly random
// 256-bit secret a brute force is already impossible, so the KDF would only buy
// CPU burn on every single authenticated request. SHA-256 plus a constant-time
// comparison is the correct trade-off here.
func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// VerifyKey reports whether plaintext hashes to expectedHash. The comparison is
// constant time so it cannot be used as a timing oracle.
func VerifyKey(plaintext, expectedHash string) bool {
	if plaintext == "" || expectedHash == "" {
		return false
	}
	actual := HashKey(plaintext)
	expected := strings.ToLower(strings.TrimSpace(expectedHash))
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

// VerifyAdminToken compares a presented admin token against the configured one
// in constant time. An unconfigured admin token denies every request rather
// than accepting an empty credential.
func VerifyAdminToken(configured, presented string) bool {
	if configured == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(configured), []byte(presented)) == 1
}

// shortenTenantID keeps the first tenantShortLen alphanumeric characters of the
// tenant id, lowercased, so that uuid/ulid identifiers produce a stable label.
func shortenTenantID(tenantID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(tenantID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() == tenantShortLen {
			break
		}
	}
	return b.String()
}

// encodeBase62 renders raw bytes in base62, left-padded so that the output
// length is constant regardless of leading zero bytes.
func encodeBase62(raw []byte) string {
	// ceil(len(raw) * 8 / log2(62)) characters are needed to represent the
	// full range; 43 characters cover 32 bytes.
	const width = 43

	n := new(big.Int).SetBytes(raw)
	base := big.NewInt(int64(len(base62Alphabet)))
	mod := new(big.Int)

	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, mod)
		out[i] = base62Alphabet[mod.Int64()]
	}
	return string(out)
}
