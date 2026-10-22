package push

import (
	"encoding/xml"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRetrySetBacksOff: a failing key is retried after each of retryDelays in
// turn, then left to the reconcile; a success forgets it.
func TestRetrySetBacksOff(t *testing.T) {
	r := newRetrySet()
	now := time.Unix(0, 0)
	for i, d := range retryDelays {
		r.record("calendar:1", true, now)
		if got := r.takeDue(now.Add(d - time.Second)); len(got) != 0 {
			t.Fatalf("retry %d due early: %v", i, got)
		}
		if got := r.takeDue(now.Add(d)); len(got) != 1 {
			t.Fatalf("retry %d not due after %s: %v", i, d, got)
		}
	}
	r.record("calendar:1", true, now) // out of delays
	if len(r.due) != 0 || len(r.attempts) != 0 {
		t.Errorf("after the last delay: due=%v attempts=%v, want both empty", r.due, r.attempts)
	}

	r.record("calendar:2", true, now)
	r.record("calendar:2", false, now)
	if len(r.due) != 0 || len(r.attempts) != 0 {
		t.Errorf("after a success: due=%v attempts=%v, want both empty", r.due, r.attempts)
	}
}

func TestTopicDeterministicAndDistinct(t *testing.T) {
	cfg := Config{TopicSecret: "s3cr3t"}
	a := cfg.Topic("calendar", "42")
	if a == "" {
		t.Fatal("empty topic")
	}
	if a != cfg.Topic("calendar", "42") {
		t.Error("topic is not stable across calls")
	}
	if a == cfg.Topic("calendar", "43") {
		t.Error("different collection id produced the same topic")
	}
	if a == cfg.Topic("addressbook", "42") {
		t.Error("different kind produced the same topic")
	}
	// A different secret must change the topic (non-enumerable without it).
	if a == (Config{TopicSecret: "other"}).Topic("calendar", "42") {
		t.Error("topic did not depend on the secret")
	}
}

func TestConfigValid(t *testing.T) {
	full := Config{VAPIDPublicKey: "p", VAPIDPrivateKey: "k", TopicSecret: "s"}
	if !full.Valid() {
		t.Error("complete config reported invalid")
	}
	for _, c := range []Config{
		{VAPIDPrivateKey: "k", TopicSecret: "s"},
		{VAPIDPublicKey: "p", TopicSecret: "s"},
		{VAPIDPublicKey: "p", VAPIDPrivateKey: "k"},
	} {
		if c.Valid() {
			t.Errorf("incomplete config %+v reported valid", c)
		}
	}
}

func TestParseRegister(t *testing.T) {
	body := `<?xml version="1.0"?>
<P:push-register xmlns:P="https://bitfire.at/webdav-push" xmlns:D="DAV:">
  <P:subscription><P:web-push-subscription>
    <P:push-resource>https://push.example/abc</P:push-resource>
    <P:subscription-public-key type="p256dh">PUBKEY</P:subscription-public-key>
    <P:auth-secret>AUTH</P:auth-secret>
  </P:web-push-subscription></P:subscription>
  <P:trigger><P:content-update><D:depth>infinity</D:depth></P:content-update></P:trigger>
</P:push-register>`
	req, err := parseRegister([]byte(body))
	if err != nil {
		t.Fatalf("parseRegister: %v", err)
	}
	if req.pushResource != "https://push.example/abc" || req.p256dh != "PUBKEY" || req.authSecret != "AUTH" {
		t.Errorf("parsed wrong values: %+v", req)
	}
}

func TestParseRegisterRejectsIncomplete(t *testing.T) {
	body := `<P:push-register xmlns:P="https://bitfire.at/webdav-push">
  <P:subscription><P:web-push-subscription>
    <P:push-resource>https://push.example/abc</P:push-resource>
  </P:web-push-subscription></P:subscription>
</P:push-register>`
	if _, err := parseRegister([]byte(body)); err == nil {
		t.Error("expected an error for a body missing the keys")
	}
}

func TestRootElementName(t *testing.T) {
	name, err := rootElementName([]byte(`<P:push-register xmlns:P="https://bitfire.at/webdav-push"/>`))
	if err != nil {
		t.Fatal(err)
	}
	if want := (xml.Name{Space: NS, Local: "push-register"}); name != want {
		t.Errorf("got %v, want %v", name, want)
	}
}

func TestBuildPushMessage(t *testing.T) {
	msg := string(buildPushMessage("TOPIC123", "http://sabre.io/ns/sync/42"))
	if !strings.Contains(msg, "<topic>TOPIC123</topic>") {
		t.Errorf("message missing topic: %s", msg)
	}
	if !strings.Contains(msg, "http://sabre.io/ns/sync/42") {
		t.Errorf("message missing sync token: %s", msg)
	}
	if !strings.Contains(msg, `xmlns="`+NS+`"`) {
		t.Errorf("message not self-namespaced: %s", msg)
	}
}

func TestWebPushTransport(t *testing.T) {
	want := `<web-push xmlns="` + NS + `"><vapid-public-key type="p256ecdsa">K&lt;&amp;</vapid-public-key></web-push>`
	if got := webPushTransport("K<&"); got != want {
		t.Errorf("webPushTransport = %s, want %s", got, want)
	}
}

func TestSplitKey(t *testing.T) {
	cases := []struct {
		in       string
		kind, id string
		ok       bool
	}{
		{"calendar:42", "calendar", "42", true},
		{"addressbook:7", "addressbook", "7", true},
		{"calendar:", "", "", false},
		{"bogus:1", "", "", false},
		{"nocolon", "", "", false},
	}
	for _, c := range cases {
		k, id, ok := splitKey(c.in)
		if ok != c.ok || k != c.kind || id != c.id {
			t.Errorf("splitKey(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, k, id, ok, c.kind, c.id, c.ok)
		}
	}
}

func TestTopicHeaderCapped(t *testing.T) {
	long := strings.Repeat("a", 50)
	if got := topicHeader(long); len(got) != 32 {
		t.Errorf("topic header not capped to 32: len=%d", len(got))
	}
	if got := topicHeader("short"); got != "short" {
		t.Errorf("short topic header altered: %q", got)
	}
}

// TestHandleMethodPassThrough verifies a non-push POST body is handed back to
// the dispatch chain intact (not consumed), so caldav/sharing still work.
func TestHandleMethodPassThrough(t *testing.T) {
	p := &Plugin{}

	// A sharing POST (not a push-register): must pass through untouched.
	shareBody := `<cs:share xmlns:cs="http://calendarserver.org/ns/"/>`
	r := httptest.NewRequest("POST", "/calendars/alice/home/", strings.NewReader(shareBody))
	w := httptest.NewRecorder()
	handled, err := p.HandleMethod(w, r)
	if err != nil {
		t.Fatalf("HandleMethod: %v", err)
	}
	if handled {
		t.Fatal("push plugin claimed a non-push POST")
	}
	got, _ := io.ReadAll(r.Body)
	if string(got) != shareBody {
		t.Errorf("body not restored intact: got %q", got)
	}

	// A non-POST method is never ours.
	r2 := httptest.NewRequest("PROPFIND", "/calendars/alice/home/", nil)
	if handled, _ := p.HandleMethod(w, r2); handled {
		t.Error("push plugin claimed a non-POST method")
	}
}
