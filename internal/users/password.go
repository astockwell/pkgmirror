package users

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. Tuned to ~50ms on a 2024-era laptop CPU; high
// enough to deter an offline brute-force against the cookie store, low
// enough to keep login fast for legitimate users. If you bump these,
// existing hashes keep verifying because the parameter values are
// encoded in the hash string.
//
// References:
//   - RFC 9106 (Argon2 spec)
//   - PHC password hashing best-practices
const (
	argon2Memory  = 64 * 1024 // 64 MiB
	argon2Time    = 1
	argon2Threads = 4
	argon2KeyLen  = 32
	argon2SaltLen = 16
)

// argon2idPrefix is the standard PHC encoding identifier.
const argon2idPrefix = "$argon2id$"

// ErrPasswordMismatch is returned by VerifyPassword when the supplied
// plaintext does not match the stored hash. Distinct from "no user" so
// callers can map both to the same opaque user-facing error message
// (without leaking which one occurred).
var ErrPasswordMismatch = errors.New("password mismatch")

// ErrNoPassword is returned by VerifyPassword when the user exists but
// has no password set (e.g. proxy-header mode, or a brand-new admin
// who hasn't run through the set-password flow yet).
var ErrNoPassword = errors.New("user has no password set")

// HashPassword returns the PHC-format argon2id encoding of plaintext.
// The salt is freshly generated per call (crypto/rand). The returned
// string is what gets stored in users.password_hash.
//
// Format:
//
//	$argon2id$v=19$m=65536,t=1,p=4$<base64-salt>$<base64-hash>
func HashPassword(plaintext string) (string, error) {
	if plaintext == "" {
		return "", errors.New("password is empty")
	}
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("argon2id: read salt: %w", err)
	}
	hash := argon2.IDKey([]byte(plaintext), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
	encoded := fmt.Sprintf("%sv=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2idPrefix,
		argon2.Version,
		argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
	return encoded, nil
}

// VerifyHash returns nil if plaintext matches the stored encoded hash,
// or ErrPasswordMismatch otherwise. Errors other than mismatch indicate
// the hash itself is malformed (boot-time corruption) and should be
// logged loudly but never let into the verify path.
func VerifyHash(encoded, plaintext string) error {
	if !strings.HasPrefix(encoded, argon2idPrefix) {
		return fmt.Errorf("argon2id: unrecognized hash format")
	}
	rest := strings.TrimPrefix(encoded, argon2idPrefix)
	parts := strings.Split(rest, "$")
	// Expect: "v=19", "m=...,t=...,p=...", "<salt>", "<hash>"
	if len(parts) != 4 {
		return fmt.Errorf("argon2id: malformed hash (got %d segments)", len(parts))
	}
	var version int
	if _, err := fmt.Sscanf(parts[0], "v=%d", &version); err != nil {
		return fmt.Errorf("argon2id: parse version: %w", err)
	}
	if version != argon2.Version {
		return fmt.Errorf("argon2id: version mismatch (stored=%d, ours=%d)", version, argon2.Version)
	}
	var memory uint32
	var time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[1], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return fmt.Errorf("argon2id: parse params: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("argon2id: decode salt: %w", err)
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return fmt.Errorf("argon2id: decode hash: %w", err)
	}

	got := argon2.IDKey([]byte(plaintext), salt, time, memory, threads, uint32(len(expected)))
	if subtle.ConstantTimeCompare(got, expected) == 1 {
		return nil
	}
	return ErrPasswordMismatch
}
