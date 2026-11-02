package passwd

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

const password = "s3cret-Passw0rd"

func bcryptHash(t *testing.T) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func ssha512Hash(salt string) []byte {
	h := sha512.New()
	h.Write([]byte(password))
	h.Write([]byte(salt))
	return append(h.Sum(nil), salt...)
}

func sha512Hash() []byte {
	h := sha512.New()
	h.Write([]byte(password))
	return h.Sum(nil)
}

func TestVerifyMatch(t *testing.T) {
	bc := bcryptHash(t)
	tests := []struct {
		name, hash string
	}{
		{"bare bcrypt", bc},
		{"BLF-CRYPT", "{BLF-CRYPT}" + bc},
		{"BLF-CRYPT lowercase scheme", "{blf-crypt}" + bc},
		{"SSHA512 b64", "{SSHA512}" + base64.StdEncoding.EncodeToString(ssha512Hash("salt"))},
		{"SSHA512 explicit b64", "{SSHA512.b64}" + base64.StdEncoding.EncodeToString(ssha512Hash("salt"))},
		{"SSHA512 base64 suffix", "{SSHA512.base64}" + base64.StdEncoding.EncodeToString(ssha512Hash("salt"))},
		{"SSHA512 hex lowercase", "{SSHA512.hex}" + hex.EncodeToString(ssha512Hash("salt"))},
		{"SSHA512 HEX uppercase", "{SSHA512.HEX}" + hex.EncodeToString(ssha512Hash("salt"))},
		{"SSHA512 lowercase scheme", "{ssha512}" + base64.StdEncoding.EncodeToString(ssha512Hash("salt"))},
		{"SHA512 b64", "{SHA512}" + base64.StdEncoding.EncodeToString(sha512Hash())},
		{"SHA512 explicit b64", "{SHA512.b64}" + base64.StdEncoding.EncodeToString(sha512Hash())},
		{"SHA512 hex lowercase", "{SHA512.hex}" + hex.EncodeToString(sha512Hash())},
		{"SHA512 HEX uppercase", "{SHA512.HEX}" + hex.EncodeToString(sha512Hash())},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Verify(tt.hash, password); err != nil {
				t.Errorf("Verify(%q) = %v, want nil", tt.hash, err)
			}
			if err := Verify(tt.hash, "wrong"); !errors.Is(err, ErrMismatch) {
				t.Errorf("Verify(%q, wrong) = %v, want ErrMismatch", tt.hash, err)
			}
		})
	}
}

func TestVerifyUnsupportedScheme(t *testing.T) {
	for _, hash := range []string{
		"{MD5-CRYPT}$1$salt$digest",
		"{CRYPT}$2y$10$abcdefghijklmnopqrstuv", // Dovecot {CRYPT}-wrapped bcrypt is not recognized
		"{SSHA512.plain}whatever",              // unknown encoding suffix
		"{BLF-CRYPT.b64}whatever",              // encoding suffix on a crypt-format scheme
		"plaintext",
		"{unterminated",
		"",
		"{}$2y$10$abcdefghijklmnopqrstuv", // empty scheme prefix is not a bare bcrypt hash
	} {
		if err := Verify(hash, password); !errors.Is(err, ErrUnsupportedScheme) {
			t.Errorf("Verify(%q) = %v, want ErrUnsupportedScheme", hash, err)
		}
	}
}

func TestVerifyMalformed(t *testing.T) {
	tests := []struct {
		hash, wantSubstr string
	}{
		{"{SSHA512}!!!not-base64!!!", "SSHA512"},
		{"{SSHA512.hex}zzzz", "SSHA512"},
		{"{SSHA512}" + base64.StdEncoding.EncodeToString([]byte("too short")), "SSHA512"},
		{"{SHA512}" + base64.StdEncoding.EncodeToString([]byte("wrong length")), "SHA512"},
		{"$2a$xx$garbage", "bcrypt"},
	}
	for _, tt := range tests {
		err := Verify(tt.hash, password)
		if err == nil || errors.Is(err, ErrMismatch) || errors.Is(err, ErrUnsupportedScheme) {
			t.Errorf("Verify(%q) = %v, want a structural error", tt.hash, err)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantSubstr) {
			t.Errorf("Verify(%q) error %q lacks scheme context %q", tt.hash, err, tt.wantSubstr)
		}
	}
}

func TestVerifyAny(t *testing.T) {
	good := "{SSHA512}" + base64.StdEncoding.EncodeToString(ssha512Hash("salt"))
	corrupt := "{SSHA512}!!!corrupt!!!"
	unknown := "{MD5-CRYPT}$1$x$y"

	// Diagnostics must cover hashes after a match: the corrupt and unknown
	// entries follow the matching one.
	res := VerifyAny([]string{good, corrupt, unknown}, password)
	if !res.Matched {
		t.Error("Matched = false, want true")
	}
	if res.Unsupported != 1 {
		t.Errorf("Unsupported = %d, want 1", res.Unsupported)
	}
	if len(res.Malformed) != 1 || !strings.Contains(res.Malformed[0], "hash 2") {
		t.Errorf("Malformed = %v, want one entry naming hash 2", res.Malformed)
	}

	res = VerifyAny([]string{good}, "wrong")
	if res.Matched || res.Unsupported != 0 || len(res.Malformed) != 0 {
		t.Errorf("plain mismatch: got %+v, want zero-value diagnostics", res)
	}

	res = VerifyAny(nil, password)
	if res.Matched {
		t.Error("Matched = true for empty hash list, want false")
	}
}
