package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePasswdFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPasswdFileCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "passwd")
	writePasswdFile(t, path, "# accounts\n\n"+
		"User@Example.com:{BLF-CRYPT}"+bcryptHashT(t, "s3cret")+"::::::\n"+
		"user@example.com:{BLF-CRYPT}"+bcryptHashT(t, "shadowed")+"\n"+ // duplicate: first wins
		"nopass@example.com::1000:1000\n")
	b, err := NewPasswdFileBackend("Realm", path, 0)
	if err != nil {
		t.Fatal(err)
	}

	principal, ok, reason := check(b, basicReq(t, "user@EXAMPLE.com", "s3cret"))
	if !ok {
		t.Fatalf("Check failed: %s", reason)
	}
	if principal != "principals/user@example.com" {
		t.Errorf("principal = %q, want principals/user@example.com", principal)
	}
	for _, c := range []struct{ user, pass string }{
		{"user@example.com", "wrong"},
		{"user@example.com", "shadowed"},
		{"nopass@example.com", ""},
		{"ghost@example.com", "s3cret"},
	} {
		if _, ok, reason := check(b, basicReq(t, c.user, c.pass)); ok {
			t.Errorf("Check(%q, %q) succeeded", c.user, c.pass)
		} else if reason != "Username or password was incorrect" {
			t.Errorf("Check(%q, %q) reason = %q, want the generic message", c.user, c.pass, reason)
		}
	}
}

func TestPasswdFileReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "passwd")
	writePasswdFile(t, path, "a@example.com:"+bcryptHashT(t, "old")+"\n")
	b, err := NewPasswdFileBackend("Realm", path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := check(b, basicReq(t, "a@example.com", "old")); !ok {
		t.Fatal("initial password rejected")
	}

	writePasswdFile(t, path, "a@example.com:"+bcryptHashT(t, "new")+"\n")
	// Force a different mtime and skip the stat throttle.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	b.file.checked = time.Time{}
	if _, ok, _ := check(b, basicReq(t, "a@example.com", "new")); !ok {
		t.Error("new password rejected after the file changed")
	}

	// A file that disappears keeps serving the last good contents.
	os.Remove(path)
	b.file.checked = time.Time{}
	if _, ok, _ := check(b, basicReq(t, "a@example.com", "new")); !ok {
		t.Error("login failed after the file vanished; want the last loaded contents")
	}
}

func TestPasswdFileMissingAtStartup(t *testing.T) {
	if _, err := NewPasswdFileBackend("Realm", filepath.Join(t.TempDir(), "nope"), 0); err == nil {
		t.Fatal("NewPasswdFileBackend succeeded for a missing file")
	}
}
