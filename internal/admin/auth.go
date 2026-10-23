package admin

import (
	"bufio"
	"calendsd/internal/auth"
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
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

// decoyHash picks a real stored hash to compare against when the username is
// unknown, so a request for a missing user costs exactly the bcrypt work of a
// wrong password for an existing one. A generated dummy at bcrypt's default
// cost (10) was a 32x timing tell against htpasswd -B's default cost 5
// (audit 5.8). Deterministic (lowest username) so the cost does not vary
// between requests either.
func decoyHash(creds map[string]string) string {
	best := ""
	for user := range creds {
		if best == "" || user < best {
			best = user
		}
	}
	return creds[best]
}

// adminThrottle is the panel's failure budget: the same shape as the DAV
// limiter, keyed by client address and by attempted username.
func adminThrottle() (byAddress, byUser *auth.Throttle) {
	return &auth.Throttle{Rate: 10.0 / 60, Burst: 20}, &auth.Throttle{Rate: 20.0 / 300, Burst: 20}
}

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
// loopback/TLS-only deployment note in the plan. Failed attempts are
// throttled per client address and per username (audit 5.8).
func BasicAuth(creds map[string]string, next http.Handler) http.Handler {
	byAddress, byUser := adminThrottle()
	return basicAuthWith(creds, byAddress, byUser, next)
}

// basicAuthWith is BasicAuth with the failure budgets supplied (tests pin
// their clocks).
func basicAuthWith(creds map[string]string, byAddress, byUser *auth.Throttle, next http.Handler) http.Handler {
	decoy := decoyHash(creds)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		// Same keying as the DAV limiter: a loopback caller without
		// X-Forwarded-For has no address key, so operators on the loopback
		// panel are throttled per username, not all together.
		addrKey, _ := auth.ClientAddress(r)
		userKey := strings.ToLower(user)
		if ok {
			wait := byAddress.RetryAfter(addrKey)
			if w := byUser.RetryAfter(userKey); w > wait {
				wait = w
			}
			if wait > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
				http.Error(w, "too many failed login attempts", http.StatusTooManyRequests)
				return
			}
		}
		hash, known := creds[user]
		if !known {
			hash = decoy
		}
		err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass))
		if !ok || !known || err != nil {
			if ok {
				byAddress.Fail(addrKey)
				byUser.Fail(userKey)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="calendsd-admin", charset="UTF-8"`)
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
