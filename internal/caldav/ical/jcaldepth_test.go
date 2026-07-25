package ical

import (
	"strings"
	"testing"
)

// TestDecodeJCalRejectsDeepNesting verifies the jCal decoder rejects a
// deeply-nested component tree (stack-exhaustion DoS) while still accepting a
// well-formed shallow document.
func TestDecodeJCalRejectsDeepNesting(t *testing.T) {
	inner := `["vevent",[],[]]`
	for i := 0; i < 300; i++ {
		inner = `["vevent",[],[` + inner + `]]`
	}
	body := `["vcalendar",[],[` + inner + `]]`
	if _, err := DecodeJCal([]byte(body)); err == nil {
		t.Fatal("expected deeply-nested jCal to be rejected")
	} else if !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("error = %v, want a nesting-depth error", err)
	}

	ok := `["vcalendar",[["version",{},"text","2.0"]],[["vevent",[["uid",{},"text","x"]],[]]]]`
	if _, err := DecodeJCal([]byte(ok)); err != nil {
		t.Fatalf("valid shallow jCal rejected: %v", err)
	}
}
