package dav

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Bug: ErrInvalidSyncToken carries no Precondition, so the error body lacks
// the {DAV:}valid-sync-token precondition element RFC 6578 §3.2 requires (and
// sabre emits).
func TestInvalidSyncTokenEmitsValidSyncTokenPrecondition(t *testing.T) {
	sc := &memSyncColl{memColl: memColl{name: "cal"}, token: "1"}
	s := NewServer("/", newMemColl("root", sc))
	s.AddPlugin(&SyncPlugin{})

	body := `<?xml version="1.0"?>
<d:sync-collection xmlns:d="DAV:">
 <d:sync-token>bogus-token</d:sync-token>
 <d:sync-level>1</d:sync-level>
 <d:prop><d:getetag/></d:prop>
</d:sync-collection>`
	req := httptest.NewRequest("REPORT", "/cal/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("sync-collection with bogus token = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if out := rec.Body.String(); !strings.Contains(out, "valid-sync-token") {
		t.Errorf("error body missing the {DAV:}valid-sync-token precondition element:\n%s", out)
	}
}
