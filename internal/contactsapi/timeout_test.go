package contactsapi_test

// RequestTimeout tests. They need no Postgres: a stalled database is a
// listener that accepts the connection and never answers the startup message,
// which is what a pg-web node silently dropping traffic looks like to pgx.

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"calserver/internal/auth"
	"calserver/internal/contactsapi"
	"calserver/internal/storage"
)

// stalledDB returns a *sql.DB whose every query hangs until its context ends.
func stalledDB(t *testing.T) *sql.DB {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	db, err := storage.Open("postgres://u@" + ln.Addr().String() + "/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// stallAuth blocks until the request's context ends, like an rcptd lookup or
// a first-login provisioning write stuck on the network.
type stallAuth struct{}

func (stallAuth) Check(r *http.Request) (string, bool, string) {
	<-r.Context().Done()
	return "", false, "authentication service is unreachable"
}

func (stallAuth) Challenge(h http.Header) { h.Add("WWW-Authenticate", `Basic realm="test"`) }

func serveTimed(t *testing.T, h http.Handler) (*httptest.ResponseRecorder, time.Duration) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/contacts?offset=0&limit=50&search=", nil)
	req.SetBasicAuth("alice@example.org", "pw")
	w := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(w, req)
	return w, time.Since(start)
}

// A stalled authentication answers 503 at the deadline — not a 401 that
// would make webmail re-prompt for a password that is fine.
func TestStalledAuthAnswers503(t *testing.T) {
	h := contactsapi.New(nil, stallAuth{}, nil, contactsapi.Options{RequestTimeout: 100 * time.Millisecond})
	w, took := serveTimed(t, h)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", w.Code, w.Body)
	}
	if w.Header().Get("WWW-Authenticate") != "" {
		t.Error("a timed-out check must not challenge for credentials")
	}
	if took > 2*time.Second {
		t.Errorf("took %v; the deadline did not bound the request", took)
	}
}

// A stalled database answers 503 at the deadline, through both the soft-delete
// gate (which fails open) and the card read behind it.
func TestStalledDatabaseAnswers503(t *testing.T) {
	db := stalledDB(t)
	users := &storage.Users{DB: db}
	tomb := func(ctx context.Context, p string) bool { ok, _ := users.IsDeletedContext(ctx, p); return ok }
	h := contactsapi.New(db, fakeAuth{}, tomb, contactsapi.Options{RequestTimeout: 200 * time.Millisecond})
	w, took := serveTimed(t, h)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", w.Code, w.Body)
	}
	if took > 2*time.Second {
		t.Errorf("took %v; the deadline did not bound the request", took)
	}
}

// downAuth answers the way RcptdBackend does when rcptd cannot be reached.
type downAuth struct{}

func (downAuth) Check(*http.Request) (string, bool, string) {
	return "", false, auth.ReasonUnreachable
}

func (downAuth) Challenge(h http.Header) { h.Add("WWW-Authenticate", `Basic realm="test"`) }

// An rcptd outage is a 503 without a challenge: webmail must not re-prompt
// for a password that was never checked.
func TestAuthOutageAnswers503(t *testing.T) {
	h := contactsapi.New(nil, downAuth{}, nil, contactsapi.Options{})
	w, _ := serveTimed(t, h)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("status = %d, challenge %q; want 503 without a challenge", w.Code, w.Header().Get("WWW-Authenticate"))
	}
}
