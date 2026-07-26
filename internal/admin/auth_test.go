package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func writeHTPasswd(t *testing.T, lines string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "htpasswd")
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func bcryptHash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func TestLoadHTPasswd(t *testing.T) {
	hash := bcryptHash(t, "s3cret")
	creds, err := LoadHTPasswd(writeHTPasswd(t, "# comment\n\nroot:"+hash+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if creds["root"] != hash {
		t.Fatalf("creds = %v, want root entry", creds)
	}

	if _, err := LoadHTPasswd(writeHTPasswd(t, "root:$apr1$abcdefgh$012345678901234567890\n")); err == nil {
		t.Fatal("MD5 htpasswd entry accepted; want an error (bcrypt only)")
	}
	if _, err := LoadHTPasswd(writeHTPasswd(t, "not-a-credential-line\n")); err == nil {
		t.Fatal("malformed line accepted; want an error")
	}
	if _, err := LoadHTPasswd(writeHTPasswd(t, "\n# only comments\n")); err == nil {
		t.Fatal("empty credential file accepted; want an error")
	}
}

func TestBasicAuth(t *testing.T) {
	creds := map[string]string{"root": bcryptHash(t, "s3cret")}
	var reached bool
	h := BasicAuth(creds, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	get := func(user, pass string, withAuth bool) int {
		reached = false
		req := httptest.NewRequest("GET", "/", nil)
		if withAuth {
			req.SetBasicAuth(user, pass)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}

	if code := get("", "", false); code != http.StatusUnauthorized {
		t.Fatalf("no credentials: %d, want 401", code)
	}
	if code := get("root", "wrong", true); code != http.StatusUnauthorized || reached {
		t.Fatalf("wrong password: %d reached=%t, want 401 unreached", code, reached)
	}
	if code := get("nobody", "s3cret", true); code != http.StatusUnauthorized || reached {
		t.Fatalf("unknown user: %d reached=%t, want 401 unreached", code, reached)
	}
	if code := get("root", "s3cret", true); code != http.StatusOK || !reached {
		t.Fatalf("valid credentials: %d reached=%t, want 200 reached", code, reached)
	}
}
