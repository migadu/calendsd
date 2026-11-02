package auth

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// passwdFile is a Dovecot passwd-file (https://doc.dovecot.org/configuration_manual/authentication/passwd_file/)
// read as a hash source for the hash-verifying backend: one
// "user:{SCHEME}hash[:uid:gid:gecos:home:shell:extra]" line per account,
// '#' comments and blank lines ignored. Only the first two fields are used.
// The file is re-read when its size or mtime changes, checked at most once
// per statEvery, so `doveadm pw` edits take effect without a restart.
type passwdFile struct {
	path string

	mu      sync.Mutex
	checked time.Time
	modTime time.Time
	size    int64
	hashes  map[string]string
}

// statEvery bounds how often a lookup stats the file for changes.
const statEvery = time.Second

func openPasswdFile(path string) (*passwdFile, error) {
	f := &passwdFile{path: path}
	if err := f.refresh(time.Now()); err != nil {
		return nil, err
	}
	return f, nil
}

// lookup answers in rcptd's terms (200 found, 404 unknown) so verify treats
// both sources alike. A reload failure after a successful load keeps serving
// the previous contents — a half-written file must not lock everyone out —
// and is reported as unavailable only if nothing was ever loaded.
func (f *passwdFile) lookup(user string) (hashes []string, status int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if now := time.Now(); now.Sub(f.checked) >= statEvery {
		if err := f.refresh(now); err != nil && f.hashes == nil {
			return nil, 0, err
		}
	}
	h, ok := f.hashes[user]
	if !ok {
		return nil, http.StatusNotFound, nil
	}
	return []string{h}, http.StatusOK, nil
}

// refresh re-reads the file if it changed; f.mu is held (or f is not yet
// shared).
func (f *passwdFile) refresh(now time.Time) error {
	f.checked = now
	st, err := os.Stat(f.path)
	if err != nil {
		return fmt.Errorf("passwd-file: %w", err)
	}
	if f.hashes != nil && st.ModTime().Equal(f.modTime) && st.Size() == f.size {
		return nil
	}
	hashes, err := parsePasswdFile(f.path)
	if err != nil {
		return err
	}
	f.hashes, f.modTime, f.size = hashes, st.ModTime(), st.Size()
	return nil
}

// parsePasswdFile maps each lowercased user to its password field. An empty
// password field is skipped (Dovecot treats it as "no password", which can
// never match), and a repeated user keeps its first line, as Dovecot does.
func parsePasswdFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("passwd-file: %w", err)
	}
	defer file.Close()
	hashes := map[string]string{}
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		user, rest, ok := strings.Cut(line, ":")
		if !ok || user == "" {
			continue
		}
		pass, _, _ := strings.Cut(rest, ":")
		user = strings.ToLower(user)
		if _, dup := hashes[user]; pass == "" || dup {
			continue
		}
		hashes[user] = pass
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("passwd-file %s: %w", path, err)
	}
	return hashes, nil
}

// NewPasswdFileBackend builds a Basic backend that verifies logins against a
// Dovecot passwd-file at path, with the same credential cache, CPU bound and
// unknown-user timing equalization as the rcptd backend. The file is read
// once here so a missing or unreadable file fails at startup.
func NewPasswdFileBackend(realm, path string, ttl time.Duration) (*RcptdBackend, error) {
	f, err := openPasswdFile(path)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("auth: reading CSPRNG for passwd-file cache key: " + err.Error())
	}
	return &RcptdBackend{
		Realm: realm,
		file:  f,
		ttl:   ttl,
		key:   key,
		cache: map[string]cacheEntry{},
	}, nil
}
