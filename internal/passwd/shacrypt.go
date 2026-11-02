package passwd

import (
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"errors"
	"hash"
	"strconv"
	"strings"
)

// SHA-crypt ($5$ SHA256-CRYPT, $6$ SHA512-CRYPT) per Ulrich Drepper's
// "Unix crypt using SHA-256 and SHA-512"
// (https://www.akkadia.org/drepper/SHA-crypt.txt), the default scheme of
// `doveadm pw` before bcrypt and of most Linux /etc/shadow files.

const (
	shaCryptSaltMax       = 16
	shaCryptRoundsDefault = 5000
	shaCryptRoundsMin     = 1000
	shaCryptRoundsMax     = 999999999
)

// shaCryptVariant is one of the two digests with its output permutation: the
// digest bytes are encoded three at a time in this order, each triple as
// four crypt-base64 characters, the final group padded per the spec.
type shaCryptVariant struct {
	prefix string
	new    func() hash.Hash
	order  [][3]int // full triples
	tail   [3]int   // final partial group (first index may repeat 0)
	tailN  int      // characters for the final group
}

var sha256Crypt = shaCryptVariant{
	prefix: "$5$",
	new:    sha256.New,
	order: [][3]int{
		{0, 10, 20}, {21, 1, 11}, {12, 22, 2}, {3, 13, 23}, {24, 4, 14},
		{15, 25, 5}, {6, 16, 26}, {27, 7, 17}, {18, 28, 8}, {9, 19, 29},
	},
	tail:  [3]int{-1, 31, 30},
	tailN: 3,
}

var sha512Crypt = shaCryptVariant{
	prefix: "$6$",
	new:    sha512.New,
	order: [][3]int{
		{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4},
		{47, 5, 26}, {6, 27, 48}, {28, 49, 7}, {50, 8, 29}, {9, 30, 51},
		{31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13}, {56, 14, 35},
		{15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19},
		{62, 20, 41},
	},
	tail:  [3]int{-1, -1, 63},
	tailN: 2,
}

// verifyShaCrypt checks a "$5$…" or "$6$…" hash, "rounds=N$" optional.
func verifyShaCrypt(hashed, password string) error {
	var v *shaCryptVariant
	switch {
	case strings.HasPrefix(hashed, sha256Crypt.prefix):
		v = &sha256Crypt
	case strings.HasPrefix(hashed, sha512Crypt.prefix):
		v = &sha512Crypt
	default:
		return ErrUnsupportedScheme
	}
	rest := hashed[len(v.prefix):]
	rounds := shaCryptRoundsDefault
	if r, ok := strings.CutPrefix(rest, "rounds="); ok {
		n, after, found := strings.Cut(r, "$")
		if !found {
			return errors.New("invalid SHA-crypt hash: unterminated rounds")
		}
		parsed, err := strconv.ParseUint(n, 10, 64)
		if err != nil {
			return errors.New("invalid SHA-crypt hash: bad rounds")
		}
		rounds = int(min(max(parsed, shaCryptRoundsMin), shaCryptRoundsMax))
		rest = after
	}
	salt, encoded, found := strings.Cut(rest, "$")
	if !found || encoded == "" {
		return errors.New("invalid SHA-crypt hash: missing digest")
	}
	if len(salt) > shaCryptSaltMax {
		salt = salt[:shaCryptSaltMax]
	}
	got := v.encode(v.sum([]byte(password), []byte(salt), rounds))
	if subtle.ConstantTimeCompare([]byte(got), []byte(encoded)) != 1 {
		return ErrMismatch
	}
	return nil
}

// sum is the digest computation of the spec, steps 1–21.
func (v *shaCryptVariant) sum(pass, salt []byte, rounds int) []byte {
	h := v.new()

	// Digest B = H(pass salt pass).
	h.Write(pass)
	h.Write(salt)
	h.Write(pass)
	b := h.Sum(nil)

	// Digest A.
	h.Reset()
	h.Write(pass)
	h.Write(salt)
	h.Write(repeatTo(b, len(pass)))
	for n := len(pass); n > 0; n >>= 1 {
		if n&1 != 0 {
			h.Write(b)
		} else {
			h.Write(pass)
		}
	}
	a := h.Sum(nil)

	// P sequence: H(pass × len(pass)) stretched to len(pass).
	h.Reset()
	for range len(pass) {
		h.Write(pass)
	}
	p := repeatTo(h.Sum(nil), len(pass))

	// S sequence: H(salt × (16 + A[0])) stretched to len(salt).
	h.Reset()
	for range 16 + int(a[0]) {
		h.Write(salt)
	}
	s := repeatTo(h.Sum(nil), len(salt))

	c := a
	for i := range rounds {
		h.Reset()
		if i&1 != 0 {
			h.Write(p)
		} else {
			h.Write(c)
		}
		if i%3 != 0 {
			h.Write(s)
		}
		if i%7 != 0 {
			h.Write(p)
		}
		if i&1 != 0 {
			h.Write(c)
		} else {
			h.Write(p)
		}
		c = h.Sum(c[:0:0])
	}
	return c
}

// repeatTo returns the first n bytes of src repeated end to end.
func repeatTo(src []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, src[:min(len(src), n-len(out))]...)
	}
	return out
}

const cryptAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// encode writes the digest in crypt's base64: little-endian 6-bit groups,
// bytes taken in the variant's permuted order.
func (v *shaCryptVariant) encode(d []byte) string {
	var sb strings.Builder
	put := func(b2, b1, b0 byte, n int) {
		w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for range n {
			sb.WriteByte(cryptAlphabet[w&0x3f])
			w >>= 6
		}
	}
	at := func(i int) byte {
		if i < 0 {
			return 0
		}
		return d[i]
	}
	for _, t := range v.order {
		put(d[t[0]], d[t[1]], d[t[2]], 4)
	}
	put(at(v.tail[0]), at(v.tail[1]), at(v.tail[2]), v.tailN)
	return sb.String()
}
