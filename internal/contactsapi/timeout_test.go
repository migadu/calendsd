package contactsapi_test

// RequestTimeout tests. They need no Postgres: a stalled database is a
// listener that accepts the connection and never answers the startup message,
// which is what a pg-web node silently dropping traffic looks like to pgx.

import (
	"context"
	"database/sql"
	"fmt"
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

func (stallAuth) Check(r *http.Request) (string, error) {
	<-r.Context().Done()
	return "", auth.ErrUnavailable
}

func (stallAuth) Challenge(h http.Header, _ *http.Request) {
	h.Add("WWW-Authenticate", `Basic realm="test"`)
}

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
	tomb := func(ctx context.Context, p string) bool { ok, _ := users.IsDeleted(ctx, p); return ok }
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

func (downAuth) Check(r *http.Request) (string, error) {
	// A real backend's call against the unreachable service.
	_, err := auth.NewRcptdBackend("test", "http://127.0.0.1:1", "", 0).Check(r)
	return "", err
}

func (downAuth) Challenge(h http.Header, _ *http.Request) {
	h.Add("WWW-Authenticate", `Basic realm="test"`)
}

// An rcptd outage is a 503 without a challenge: webmail must not re-prompt
// for a password that was never checked.
func TestAuthOutageAnswers503(t *testing.T) {
	h := contactsapi.New(nil, downAuth{}, nil, contactsapi.Options{})
	w, _ := serveTimed(t, h)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("status = %d, challenge %q; want 503 without a challenge", w.Code, w.Header().Get("WWW-Authenticate"))
	}
}

// A write stuck behind a row lock answers 503 at the deadline instead of
// waiting for the lock: the request's context reaches the storage write.
func TestLockedWriteAnswers503(t *testing.T) {
	e := newEnv(t, "calserver_capi_lockedwrite", contactsapi.Options{RequestTimeout: 300 * time.Millisecond})
	const alice = "alice@example.org"
	e.principal(alice)
	var created contactJSON
	e.want(e.do(alice, "POST", "/api/v1/contacts", map[string]string{"vcard": vcf("u1", "Bob Builder")}), 201).json(t, &created)

	tx, err := e.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT 1 FROM cards WHERE id = $1 FOR UPDATE`, created.ID); err != nil {
		t.Fatal(err)
	}

	done := make(chan resp, 1)
	start := time.Now()
	go func() { done <- e.do(alice, "DELETE", fmt.Sprintf("/api/v1/contacts/%d", created.ID), nil) }()
	select {
	case r := <-done:
		if r.code != http.StatusServiceUnavailable {
			t.Errorf("locked DELETE = %d, want 503; body %s", r.code, r.body)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("took %v; the deadline did not bound the write", took)
		}
	case <-time.After(5 * time.Second):
		tx.Rollback() // let the stuck request finish
		<-done
		t.Fatal("locked DELETE ignored the request deadline")
	}
}
