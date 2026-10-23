package caldav

import (
	"testing"

	"calendsd/internal/caldav/ical"
)

// The coalesce predicate casts args->>'sequence' in SQL, so the value must
// be canonical digits by the time it is enqueued (audit 5.6).
func TestEventSequenceAndDTStampCanonical(t *testing.T) {
	mk := func(seq, stamp string) *ical.Message {
		data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//t//EN\r\nMETHOD:REQUEST\r\n" +
			"BEGIN:VEVENT\r\nUID:s\r\nDTSTART:20260901T100000Z\r\n"
		if seq != "" {
			data += "SEQUENCE:" + seq + "\r\n"
		}
		if stamp != "" {
			data += "DTSTAMP" + stamp + "\r\n"
		}
		data += "END:VEVENT\r\nEND:VCALENDAR\r\n"
		cal, err := ical.Decode([]byte(data))
		if err != nil {
			t.Fatalf("%s / %s: %v", seq, stamp, err)
		}
		return &ical.Message{Calendar: cal}
	}
	for _, tc := range []struct{ seq, want string }{
		{"", "0"}, {"0", "0"}, {"7", "7"}, {"1.0", "0"}, {"-3", "0"}, {"abc", "0"},
		{"2147483648", "2147483648"}, {"99999999999999999999", "0"}, {" 12 ", "12"},
	} {
		if got := eventSequence(mk(tc.seq, ":20260101T000000Z")); got != tc.want {
			t.Errorf("SEQUENCE:%q → %q, want %q", tc.seq, got, tc.want)
		}
	}
	for _, tc := range []struct{ stamp, want string }{
		{":20260101T000000Z", "20260101T000000Z"},
		{";TZID=Europe/Zurich:20260101T010000", "20260101T000000Z"},
		{"", ""},
	} {
		if got := eventDTStamp(mk("1", tc.stamp)); got != tc.want {
			t.Errorf("DTSTAMP%s → %q, want %q", tc.stamp, got, tc.want)
		}
	}
}
