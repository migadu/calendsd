package jobs

import (
	"bytes"
	"errors"
	"testing"
)

// A sealed credential is bound to its job (worker, username, recipient):
// moving the ciphertext to another row no longer opens it (audit 6.1).
func TestCredentialBoundToJob(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	kr, err := NewKeyring(Key{ID: 1, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	args := []byte(`{"username":"alice","recipient":"bob@x.test","method":"REQUEST"}`)
	aad := credentialAAD("imip_send", args)
	ct, nonce, id, err := kr.Encrypt([]byte("hunter2"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := kr.Decrypt(ct, nonce, aad, id); err != nil || string(pt) != "hunter2" {
		t.Fatalf("round trip: %q %v", pt, err)
	}
	other := credentialAAD("imip_send", []byte(`{"username":"mallory","recipient":"bob@x.test"}`))
	if _, err := kr.Decrypt(ct, nonce, other, id); !errors.Is(err, errUndecryptable) {
		t.Errorf("ciphertext opened under another job's binding: %v", err)
	}
	if _, err := kr.Decrypt(ct, nonce, nil, id); !errors.Is(err, errUndecryptable) {
		t.Errorf("ciphertext opened with no binding: %v", err)
	}
}
