package ical

import (
	"strings"
	"testing"
	"unicode/utf8"

	goical "github.com/emersion/go-ical"
)

// EncodeLenient folded at a fixed 75 bytes, splitting multi-byte characters
// across lines (invalid UTF-8 on each), and wrote continuation lines of 76
// octets counting the leading space.
func TestEncodeLenientFoldsOnRunes(t *testing.T) {
	summary := strings.Repeat("ä€", 40) + "x"
	cal, err := Decode(event("DTSTART:20260301T090000Z", "SUMMARY:"+summary))
	if err != nil {
		t.Fatal(err)
	}
	out := string(EncodeLenient(cal))
	for _, line := range strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Errorf("line of %d octets: %q", len(line), line)
		}
		if !utf8.ValidString(line) {
			t.Errorf("line splits a character: %q", line)
		}
	}
	back, err := Decode([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Children[0].Props.Get("SUMMARY").Value; got != summary {
		t.Errorf("SUMMARY round-trip = %q, want %q", got, summary)
	}
}

// Parameter values: a double quote or line break used to be written raw,
// which ends the quoted value early, and a multi-valued parameter was written
// as the parameter repeated once per value.
func TestEncodeLenientParamValues(t *testing.T) {
	cal := goical.NewCalendar()
	ev := goical.NewComponent(goical.CompEvent)
	att := goical.NewProp("ATTENDEE")
	att.Value = "mailto:a@example.org"
	att.Params.Set("CN", "Anna \"Nan\" Doe\nSales")
	att.Params["MEMBER"] = []string{"mailto:g1@example.org", "mailto:g2@example.org"}
	ev.Props.Set(att)
	cal.Children = append(cal.Children, ev)

	out := strings.ReplaceAll(string(EncodeLenient(cal)), "\r\n ", "")
	want := `ATTENDEE;CN=Anna ^'Nan^' Doe^nSales;MEMBER="mailto:g1@example.org","mailto:g2@example.org":mailto:a@example.org`
	if !strings.Contains(out, want+"\r\n") {
		t.Errorf("want %s in:\n%s", want, out)
	}
	back, err := Decode([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Children[0].Props.Get("ATTENDEE").Params["MEMBER"]; len(got) != 2 {
		t.Errorf("MEMBER = %q, want two values", got)
	}
}
