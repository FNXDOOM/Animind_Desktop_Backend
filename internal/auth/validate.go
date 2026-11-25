package auth

import (
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
)

// Validation errors surfaced to clients; they are written for a form message, not a log line.
var (
	ErrInvalidEmail    = errors.New("email address is not valid")
	ErrInvalidUsername = errors.New("username must be 2-32 characters using letters, numbers, dot, dash or underscore")
)

const (
	minUsernameLength = 2
	maxUsernameLength = 32
	maxStoredEmailLen = 254
)

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// NormalizeEmail lowercases and trims an address. It is stored in email_normalized and is the
// key every login lookup uses, so "A@X.com" and "a@x.com" cannot coexist.
func NormalizeEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// ValidateEmail accepts a parseable single @ with a dotted domain and no whitespace. It is not
// RFC 5322-complete: deliverability is proven by sending mail, not by a regex.
func ValidateEmail(raw string) error {
	normalized := NormalizeEmail(raw)
	if normalized == "" {
		return ErrInvalidEmail
	}
	if len(normalized) > maxStoredEmailLen {
		return ErrInvalidEmail
	}
	if strings.Count(normalized, "@") != 1 {
		return ErrInvalidEmail
	}
	addr, err := mail.ParseAddress(normalized)
	if err != nil || addr.Address != normalized {
		return ErrInvalidEmail
	}
	at := strings.LastIndex(normalized, "@")
	domain := normalized[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return ErrInvalidEmail
	}
	return nil
}

// ValidateUsername enforces the display-name rules used at signup and profile update.
func ValidateUsername(raw string) error {
	name := strings.TrimSpace(raw)
	if len(name) < minUsernameLength || len(name) > maxUsernameLength {
		return ErrInvalidUsername
	}
	if !usernamePattern.MatchString(name) {
		return ErrInvalidUsername
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return ErrInvalidUsername
	}
	return nil
}

// UsernameFromEmail derives a display name from the local part when a signup omits one.
func UsernameFromEmail(email string) string {
	at := strings.Index(email, "@")
	if at <= 0 {
		return "user"
	}
	local := strings.ReplaceAll(email[:at], ".", "")
	if plus := strings.Index(local, "+"); plus >= 0 {
		local = local[:plus]
	}
	local = strings.Trim(local, "-_")
	if len(local) < minUsernameLength {
		local = local + "user"
	}
	if len(local) > maxUsernameLength {
		local = local[:maxUsernameLength]
	}
	// A local part that is not a legal username falls back to a fixed name.
	if !usernamePattern.MatchString(local) {
		return "anime-fan"
	}
	return local
}

// DefaultAvatar builds the placeholder portrait used until a user supplies their own.
func DefaultAvatar(name string) string {
	encoded := strings.ReplaceAll(name, " ", "%20")
	return fmt.Sprintf("https://ui-avatars.com/api/?name=%s&background=8b5cf6&color=fff&bold=true", encoded)
}

// TruncateLabel clamps a client-supplied device label to the column width of 128 characters.
func TruncateLabel(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max]
}
