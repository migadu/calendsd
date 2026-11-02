// Package passwd verifies passwords against Dovecot-style password hashes
// (bcrypt/BLF-CRYPT, SHA512-CRYPT, SHA256-CRYPT, SSHA512, SHA512), as served by rcptd or read from a
// Dovecot passwd-file for the hash-verifying authentication backend.
package passwd

import (
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// ErrUnsupportedScheme is returned for hash schemes this package does not
// recognize. A hash with an unknown scheme can never authenticate.
var ErrUnsupportedScheme = errors.New("unsupported hash scheme")

// ErrMismatch is returned when a well-formed hash simply does not match the
// password, as distinct from a structurally invalid/corrupt hash. Callers use
// it to tell "wrong password" apart from "unusable stored hash".
var ErrMismatch = errors.New("password does not match")

// Verify checks a password against a Dovecot-style hash. It returns nil on a
// match, ErrMismatch on a plain mismatch, ErrUnsupportedScheme for unknown
// schemes, and any other error for a structurally invalid hash.
func Verify(hashedPassword, password string) error {
	// A bare crypt-format bcrypt hash has no {SCHEME} prefix.
	if strings.HasPrefix(hashedPassword, "$2a$") ||
		strings.HasPrefix(hashedPassword, "$2b$") ||
		strings.HasPrefix(hashedPassword, "$2y$") {
		return verifyBcrypt(hashedPassword, password)
	}
	// So does a bare SHA-crypt one, as in /etc/shadow.
	if strings.HasPrefix(hashedPassword, "$5$") || strings.HasPrefix(hashedPassword, "$6$") {
		return verifyShaCrypt(hashedPassword, password)
	}
	scheme, encoding, payload := splitScheme(hashedPassword)
	switch scheme {
	case "SSHA512":
		return verifySSHA512(encoding, payload, password)
	case "SHA512":
		return verifySHA512(encoding, payload, password)
	case "BLF-CRYPT":
		// BLF-CRYPT is bcrypt in crypt text format; encoding suffixes don't apply.
		if encoding != "" {
			return ErrUnsupportedScheme
		}
		return verifyBcrypt(payload, password)
	case "SHA512-CRYPT", "SHA256-CRYPT", "CRYPT":
		// The payload names its own algorithm ($6$, $5$, or bcrypt's $2…$
		// under CRYPT); DES and MD5 crypt are not supported.
		if encoding != "" {
			return ErrUnsupportedScheme
		}
		if strings.HasPrefix(payload, "$2") && scheme == "CRYPT" {
			return verifyBcrypt(payload, password)
		}
		if (scheme == "SHA512-CRYPT" && !strings.HasPrefix(payload, "$6$")) ||
			(scheme == "SHA256-CRYPT" && !strings.HasPrefix(payload, "$5$")) {
			return ErrUnsupportedScheme
		}
		return verifyShaCrypt(payload, password)
	default:
		// Includes the empty scheme from inputs like "{}..." or no prefix at all.
		return ErrUnsupportedScheme
	}
}

// Result summarizes verifying a password against a user's full set of stored
// hashes.
type Result struct {
	Matched     bool
	Unsupported int      // hashes whose scheme Verify does not recognize
	Malformed   []string // structural errors from corrupt hashes, one per hash
}

// VerifyAny checks the password against every hash in the list. It always
// scans the full list — diagnostics cover every stored hash even when an
// early one matches, and a serving caller does not leak which position
// matched through timing.
func VerifyAny(hashes []string, password string) Result {
	var res Result
	for i, h := range hashes {
		err := Verify(h, password)
		switch {
		case err == nil:
			res.Matched = true
		case errors.Is(err, ErrMismatch):
			// ordinary wrong password — nothing to report
		case errors.Is(err, ErrUnsupportedScheme):
			res.Unsupported++
		default:
			res.Malformed = append(res.Malformed, fmt.Sprintf("hash %d: %v", i+1, err))
		}
	}
	return res
}

// splitScheme parses "{SCHEME.encoding}payload". Dovecot canonically writes
// the scheme uppercase and the encoding suffix lowercase, but real exports mix
// cases, so both are normalized. A string without a {...} prefix is returned
// with an empty scheme and the whole input as payload.
func splitScheme(hashedPassword string) (scheme, encoding, payload string) {
	if !strings.HasPrefix(hashedPassword, "{") {
		return "", "", hashedPassword
	}
	end := strings.Index(hashedPassword, "}")
	if end < 0 {
		return "", "", hashedPassword
	}
	scheme = hashedPassword[1:end]
	payload = hashedPassword[end+1:]
	if dot := strings.Index(scheme, "."); dot >= 0 {
		encoding = strings.ToLower(scheme[dot+1:])
		scheme = scheme[:dot]
	}
	return strings.ToUpper(scheme), encoding, payload
}

// verifyBcrypt compares a bcrypt hash, normalizing an ordinary mismatch to
// ErrMismatch while wrapping structural errors (e.g. a corrupt hash) with the
// scheme name.
func verifyBcrypt(hash, password string) error {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return ErrMismatch
	}
	if err != nil {
		return fmt.Errorf("invalid bcrypt hash: %w", err)
	}
	return nil
}

func verifySSHA512(encoding, payload, password string) error {
	decoded, err := decodePayload("SSHA512", encoding, payload)
	if err != nil {
		return err
	}
	// 64-byte SHA512 digest followed by a salt of at least one byte.
	if len(decoded) < 64+1 {
		return errors.New("invalid SSHA512 hash: too short")
	}
	storedHash := decoded[:64]
	salt := decoded[64:]
	h := sha512.New()
	h.Write([]byte(password))
	h.Write(salt)
	if subtle.ConstantTimeCompare(storedHash, h.Sum(nil)) != 1 {
		return ErrMismatch
	}
	return nil
}

func verifySHA512(encoding, payload, password string) error {
	storedHash, err := decodePayload("SHA512", encoding, payload)
	if err != nil {
		return err
	}
	if len(storedHash) != 64 {
		return errors.New("invalid SHA512 hash: incorrect length")
	}
	h := sha512.New()
	h.Write([]byte(password))
	if subtle.ConstantTimeCompare(storedHash, h.Sum(nil)) != 1 {
		return ErrMismatch
	}
	return nil
}

// decodePayload decodes a hash payload per its encoding suffix. Dovecot's
// default encoding for these schemes is base64; ".b64"/".base64" name it
// explicitly and ".hex" selects hex.
func decodePayload(scheme, encoding, payload string) ([]byte, error) {
	var data []byte
	var err error
	switch encoding {
	case "", "b64", "base64":
		data, err = base64.StdEncoding.DecodeString(payload)
	case "hex":
		data, err = hex.DecodeString(payload)
	default:
		return nil, ErrUnsupportedScheme
	}
	if err != nil {
		return nil, fmt.Errorf("invalid %s hash: %w", scheme, err)
	}
	return data, nil
}
