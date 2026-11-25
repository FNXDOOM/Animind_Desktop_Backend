// Package auth implements the backend's own authentication: password hashing, token issuance
// and verification, and the session lifecycle for both clients. Desktop tokens (adk_/adr_) are
// HMAC-signed and pinned in desktop_auth_sessions for instant revocation; web tokens (as_) are
// opaque and stored only as a SHA-256 hash. Passwords use argon2id in PHC string format so the
// parameters can be raised without a migration, and verification is constant-time.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Password hashing errors; none should ever be logged with the password attached.
var (
	ErrWeakPassword = errors.New("password is too weak")
	ErrInvalidHash  = errors.New("stored password hash is malformed")
	ErrHashMismatch = errors.New("password does not match")
)

const (
	minPasswordLength = 8
	maxPasswordLength = 256
)

// Argon2Params tunes the password hash; defaults follow OWASP's "second choice" (m=64MiB, t=3, p=4).
type Argon2Params struct {
	Memory  uint32
	Time    uint32
	Threads uint8
	KeyLen  uint32
}

// DefaultArgon2Params returns the parameters used when config does not override them.
func DefaultArgon2Params() Argon2Params {
	return Argon2Params{Memory: 65536, Time: 3, Threads: 4, KeyLen: 32}
}

// HashPassword derives an argon2id hash in PHC string format. The 16 fresh salt bytes keep two
// users with the same password from sharing comparable bytes in the database.
func HashPassword(password string, params Argon2Params) (string, error) {
	if err := ValidatePasswordStrength(password); err != nil {
		return "", err
	}
	if params.KeyLen == 0 {
		params.KeyLen = 32
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	dk := argon2.IDKey([]byte(password), salt, params.Time, params.Memory, params.Threads, params.KeyLen)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		params.Memory, params.Time, params.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk),
	), nil
}

// VerifyPassword checks a password against a stored hash in constant time. It re-derives using
// the parameters embedded in the hash, so hashes made under weaker settings keep working.
func VerifyPassword(password, stored string) (needsRehash bool, err error) {
	params, salt, hash, err := parseArgon2Hash(stored)
	if err != nil {
		return false, err
	}
	dk := argon2.IDKey([]byte(password), salt, params.Time, params.Memory, params.Threads, uint32(len(hash)))
	if subtle.ConstantTimeCompare(dk, hash) != 1 {
		return false, ErrHashMismatch
	}
	// Ask the caller to upgrade the stored hash when it was created with weaker parameters.
	target := DefaultArgon2Params()
	return params.Memory != target.Memory || params.Time != target.Time ||
		params.Threads != target.Threads || len(hash) != int(target.KeyLen), nil
}

// VerifyPasswordAgainstCurrent is like VerifyPassword but compares against supplied
// current parameters, which is what the login path uses.
func VerifyPasswordAgainstCurrent(password, stored string, current Argon2Params) (needsRehash bool, err error) {
	params, salt, hash, err := parseArgon2Hash(stored)
	if err != nil {
		return false, err
	}
	dk := argon2.IDKey([]byte(password), salt, params.Time, params.Memory, params.Threads, uint32(len(hash)))
	if subtle.ConstantTimeCompare(dk, hash) != 1 {
		return false, ErrHashMismatch
	}
	return params.Memory != current.Memory || params.Time != current.Time ||
		params.Threads != current.Threads || uint32(len(hash)) != current.KeyLen, nil
}

func parseArgon2Hash(stored string) (params Argon2Params, salt, hash []byte, err error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return params, nil, nil, ErrInvalidHash
	}
	if parts[1] != "v=19" {
		return params, nil, nil, ErrInvalidHash
	}
	for _, kv := range strings.Split(parts[2], ",") {
		key, value, found := strings.Cut(kv, "=")
		if !found {
			return params, nil, nil, ErrInvalidHash
		}
		n, convErr := strconv.Atoi(value)
		if convErr != nil || n < 0 {
			return params, nil, nil, ErrInvalidHash
		}
		switch key {
		case "m":
			params.Memory = uint32(n)
		case "t":
			params.Time = uint32(n)
		case "p":
			params.Threads = uint8(n)
		default:
			return params, nil, nil, ErrInvalidHash
		}
	}
	if params.Memory == 0 || params.Time == 0 || params.Threads == 0 {
		return params, nil, nil, ErrInvalidHash
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return params, nil, nil, ErrInvalidHash
	}
	hash, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return params, nil, nil, ErrInvalidHash
	}
	return params, salt, hash, nil
}

// ValidatePasswordStrength applies the cheap, user-facing rules. Length is the only
// enforced factor: composition rules push users toward predictable substitutions.
func ValidatePasswordStrength(password string) error {
	if len(password) < minPasswordLength {
		return fmt.Errorf("%w: at least %d characters", ErrWeakPassword, minPasswordLength)
	}
	if len(password) > maxPasswordLength {
		return fmt.Errorf("%w: at most %d characters", ErrWeakPassword, maxPasswordLength)
	}
	return nil
}
