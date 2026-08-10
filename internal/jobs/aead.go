package jobs

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Keyring holds the AEAD keys that protect per-user submission credentials at
// rest (D12). New rows are always encrypted with the current key; older keys
// are kept only so credentials written before a rotation stay decryptable until
// they age out (their jobs settle or the TTL sweep clears them). A ciphertext
// whose key_id is not in the ring is undecryptable → the caller fail-safes
// (discard + D5b), never falling back to an unmetered relay.
//
// The keys come from config and live out-of-band (never in Postgres, excluded
// from DB backups) — that separation is the whole security value against a
// leaked DB dump. A nil *Keyring means no key is configured: the per-user
// durable path is disabled and Encrypt/Decrypt refuse.
type Keyring struct {
	current keyEntry
	byID    map[int16]keyEntry
}

type keyEntry struct {
	id  int16
	gcm cipher.AEAD
}

// Key is one configured encryption key: a stable ID (for the key_id column and
// rotation) and 32 raw bytes (AES-256).
type Key struct {
	ID  int16
	Key []byte
}

// NewKeyring builds a ring whose current key encrypts new rows; prev keys are
// accepted for decryption only (rotation overlap). IDs must be distinct and
// every key must be exactly 32 bytes.
func NewKeyring(current Key, prev ...Key) (*Keyring, error) {
	kr := &Keyring{byID: map[int16]keyEntry{}}
	add := func(k Key) error {
		e, err := newKeyEntry(k)
		if err != nil {
			return err
		}
		if _, dup := kr.byID[e.id]; dup {
			return fmt.Errorf("jobs: duplicate key_id %d in keyring", e.id)
		}
		kr.byID[e.id] = e
		return nil
	}
	cur, err := newKeyEntry(current)
	if err != nil {
		return nil, err
	}
	kr.current = cur
	kr.byID[cur.id] = cur
	for _, k := range prev {
		if err := add(k); err != nil {
			return nil, err
		}
	}
	return kr, nil
}

func newKeyEntry(k Key) (keyEntry, error) {
	if len(k.Key) != 32 {
		return keyEntry{}, fmt.Errorf("jobs: encryption key %d must be 32 bytes, got %d", k.ID, len(k.Key))
	}
	block, err := aes.NewCipher(k.Key)
	if err != nil {
		return keyEntry{}, fmt.Errorf("jobs: key %d: %w", k.ID, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return keyEntry{}, fmt.Errorf("jobs: key %d: %w", k.ID, err)
	}
	return keyEntry{id: k.ID, gcm: gcm}, nil
}

// errUndecryptable marks a ciphertext whose key is gone (rotated out) or whose
// bytes don't authenticate. The worker maps it to a fail-safe discard (D12).
var errUndecryptable = errors.New("jobs: credential undecryptable (unknown key_id or tampered ciphertext)")

// Encrypt seals plaintext with the current key, returning the ciphertext, the
// fresh random nonce, and the key_id used. Callers persist all three columns.
func (k *Keyring) Encrypt(plaintext []byte) (ciphertext, nonce []byte, keyID int16, err error) {
	if k == nil {
		return nil, nil, 0, errors.New("jobs: no encryption key configured")
	}
	nonce = make([]byte, k.current.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, 0, fmt.Errorf("jobs: nonce: %w", err)
	}
	ciphertext = k.current.gcm.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, k.current.id, nil
}

// Decrypt opens a ciphertext with the key named by keyID. It returns
// errUndecryptable if that key is no longer in the ring or the bytes fail
// authentication — never a partial or guessed plaintext.
func (k *Keyring) Decrypt(ciphertext, nonce []byte, keyID int16) ([]byte, error) {
	if k == nil {
		return nil, errors.New("jobs: no encryption key configured")
	}
	e, ok := k.byID[keyID]
	if !ok {
		return nil, errUndecryptable
	}
	pt, err := e.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, errUndecryptable
	}
	return pt, nil
}
