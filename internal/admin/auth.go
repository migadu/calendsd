package admin

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// LoadHTPasswd reads an htpasswd file of user:hash lines (ADMIN-PLAN.md
// §2.1). Only bcrypt hashes are accepted — `htpasswd -B` output ($2y$…) —
// so a stray MD5/SHA htpasswd entry fails loudly at startup instead of
// silently never matching.
func LoadHTPasswd(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	creds := map[string]string{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		user, hash, found := strings.Cut(text, ":")
		if !found || user == "" {
			return nil, fmt.Errorf("%s:%d: not a user:hash line", path, line)
		}
		if !strings.HasPrefix(hash, "$2") {
			return nil, fmt.Errorf("%s:%d: only bcrypt hashes are supported (htpasswd -B)", path, line)
		}
		creds[user] = hash
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(creds) == 0 {
		return nil, fmt.Errorf("%s: no credentials", path)
	}
	return creds, nil
}

// dummyHash is compared against when the username is unknown, so a request
// for a missing user costs the same bcrypt work as a wrong password.
var dummyHash = func() string {
	h, err := bcrypt.GenerateFromPassword([]byte("calserver-admin-dummy"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return string(h)
}()

// actorContextKey carries the authenticated admin username so write
// endpoints can attribute their audit log lines.
type actorContextKey struct{}

// actor returns the authenticated admin username, or "unknown" when the
// handler runs without BasicAuth (tests).
func actor(r *http.Request) string {
	if v, ok := r.Context().Value(actorContextKey{}).(string); ok {
		return v
	}
	return "unknown"
}

// BasicAuth guards every admin route with HTTP Basic against the htpasswd
// credentials. Basic resends the password each request, hence the
// loopback/TLS-only deployment note in the plan.
func BasicAuth(creds map[string]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		hash, known := creds[user]
		if !known {
			hash = dummyHash
		}
		err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass))
		if !ok || !known || err != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="calserver-admin", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), actorContextKey{}, user)))
	})
}

// AuthenticatedHandler is the production entry point: the full admin mux
// guarded by BasicAuth against creds. Wiring this (rather than composing
// Handler() and BasicAuth by hand) keeps the auth wrapper from ever being
// omitted, and lets a test assert the whole surface is protected. Handler()
// alone is unauthenticated — for unit tests that exercise a single endpoint.
func (a *App) AuthenticatedHandler(creds map[string]string) http.Handler {
	return BasicAuth(creds, a.Handler())
}

// requireMutationHeader is the stateless CSRF guard: browsers attach Basic
// credentials to cross-site requests, but a cross-site form or no-CORS
// fetch cannot set a custom header. Every non-GET handler calls this first.
func requireMutationHeader(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Admin-Request") != "1" {
		http.Error(w, "missing X-Admin-Request header (CSRF guard)", http.StatusForbidden)
		return false
	}
	return true
}
