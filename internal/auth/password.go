// Package auth implements the backend's own authentication: password hashing,
// token issuance and verification, and the session lifecycle for both clients.
//
// Two token families exist because the two clients have different needs:
//
//   - Desktop (adk_/adr_): a short-lived HMAC-signed access token that can be
//     verified without a database read, paired with a rotating opaque refresh token.
//     Both are pinned in desktop_auth_sessions so revocation is instant. This format
//     is wire-compatible with the Qt desktop player already in use.
//   - Web (as_): an opaque bearer token for the site, stored only as a SHA-256 hash,
//     with sliding expiry and a remember-me lifetime.
//
// Passwords use argon2id and are encoded in the PHC string format so parameters can
// be raised later without a migration. Verification is constant-time throughout.
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

// Password hashing errors. Callers map these to 400/401 responses; none of them
// should ever be logged with the password attached.
var (
	ErrWeakPassword = errors.New("password is too weak")
	ErrInvalidHash  = errors.New("stored password hash is malformed")
	ErrHashMismatch = errors.New("password does not match")
)

const (
	minPasswordLength = 8
	maxPasswordLength = 256
)

// Argon2Params tunes the password hash. Defaults follow the OWASP recommended
// "second choice" (m=64MiB, t=3, p=4).
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

// HashPassword derives an argon2id hash encoded as:
//
//	argon2id$v=19$m=<memory>,t=<time>,p=<threads>$<b64 salt>$<b64 hash>
//
// The salt is 16 fresh random bytes. Randomness here means two users with the same
// password share no comparable bytes in the database.
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

// VerifyPassword checks a password against a stored PHC argon2id hash in constant time.
//
// It re-derives using the parameters embedded in the hash, so hashes created under
// older (weaker) settings keep working after the configured defaults are raised.
func VerifyPassword(password, stored string) (needsRehash bool, err error) {
	params, salt, hash, err := parseArgon2Hash(stored)
	if err != nil {
		return false, err
	}
	dk := argon2.IDKey([]byte(password), salt, params.Time, params.Memory, params.Threads, uint32(len(hash)))
	if subtle.ConstantTimeCompare(dk, hash) != 1 {
		return false, ErrHashMismatch
	}
	// Ask the caller to upgrade the stored hash when it was created with weaker
	// parameters than we now require.
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
