package corpus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"calendsd/internal/caldav"
	"calendsd/internal/carddav"
)

// --- corpus.go ---

func TestHeadersUnmarshalInvalidJSON(t *testing.T) {
	// Called directly: json.Unmarshal validates syntax before dispatching to
	// custom unmarshalers, so the decoder's own error path needs a raw call.
	var h Headers
	if err := h.UnmarshalJSON([]byte(`{`)); err == nil {
		t.Fatal("expected error for truncated JSON")
	}
}

func TestHeadersUnmarshalScalarValues(t *testing.T) {
	// PHP fixtures may carry bare strings or numbers instead of arrays.
	var h Headers
	if err := json.Unmarshal([]byte(`{"X-Str":"one","X-Num":42}`), &h); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := h["X-Str"]; len(got) != 1 || got[0] != "one" {
		t.Errorf("X-Str = %v, want [one]", got)
	}
	if got := h["X-Num"]; len(got) != 1 || got[0] != "42" {
		t.Errorf("X-Num = %v, want [42]", got)
	}
}

func TestLoadBadGlobPattern(t *testing.T) {
	if _, err := Load("["); err == nil {
		t.Fatal("expected glob pattern error")
	}
}

func TestLoadUnreadableEntry(t *testing.T) {
	// A directory named *.json matches the glob but cannot be read as a file.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "not-a-file.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected read error for directory entry")
	}
}

func TestLoadMalformedRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
	if !strings.Contains(err.Error(), "bad.json") {
		t.Errorf("error %q should name the failing file", err)
	}
}

// --- icscanon.go ---

func TestCanonicalizeICSUnparseableRoot(t *testing.T) {
	// Mentions VCALENDAR but the first line is not a BEGIN, so the parser
	// bails and the text passes through untouched.
	in := "X-JUNK:1\nBEGIN:VCALENDAR\nEND:VCALENDAR\n"
	if got := canonicalizeICS(in); got != in {
		t.Errorf("canonicalizeICS = %q, want input unchanged", got)
	}
}

func TestCanonicalizeICSUnterminatedComponent(t *testing.T) {
	// Missing END line: the parser returns the partial component at EOF and
	// canonicalization closes it.
	in := "BEGIN:VCALENDAR\nSUMMARY:x"
	want := "BEGIN:VCALENDAR\nSUMMARY:x\nEND:VCALENDAR\n"
	if got := canonicalizeICS(in); got != want {
		t.Errorf("canonicalizeICS = %q, want %q", got, want)
	}
}

// --- seed.go: BuildServer error paths ---

func TestBuildServerErrors(t *testing.T) {
	validTree := &Tree{Type: "collection", Name: "root"}
	tests := []struct {
		name string
		rec  Record
	}{
		{"bad principals fixture", Record{
			Setup:   Setup{Principals: json.RawMessage(`{not json`)},
			Request: Request{Tree: validTree},
		}},
		{"bad caldav fixture", Record{
			Setup:   Setup{SetupCalDAV: true, Calendars: json.RawMessage(`{not json`)},
			Request: Request{Tree: validTree},
		}},
		{"bad carddav fixture", Record{
			Setup:   Setup{SetupCardDAV: true, AddressBooks: json.RawMessage(`{not json`)},
			Request: Request{Tree: validTree},
		}},
		{"missing tree", Record{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildServer(&tc.rec); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestBuildServerPropertyStorage(t *testing.T) {
	rec := Record{
		Setup:   Setup{SetupPropertyStorage: true},
		Request: Request{Tree: &Tree{Type: "collection", Name: "root"}},
	}
	if _, err := BuildServer(&rec); err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
}

// --- seed.go: seedCardDAV ---

func TestSeedCardDAVBadCards(t *testing.T) {
	rec := &Record{Setup: Setup{Cards: json.RawMessage(`{not json`)}}
	if _, err := seedCardDAV(rec); err == nil {
		t.Fatal("expected error for malformed carddavCards")
	}
}

func TestSeedCardDAVCardRowMap(t *testing.T) {
	// A card value may be a row map with a "carddata" key rather than a
	// bare vCard string.
	rec := &Record{Setup: Setup{
		AddressBooks: json.RawMessage(`[{"id":1,"uri":"book1","principaluri":"principals/user1","{DAV:}displayname":"Book","ignored-non-clark":"x"}]`),
		Cards:        json.RawMessage(`{"1":{"card1.vcf":{"carddata":"BEGIN:VCARD\nEND:VCARD\n"},"card2.vcf":"BEGIN:VCARD\nEND:VCARD\n"}}`),
	}}
	backend, err := seedCardDAV(rec)
	if err != nil {
		t.Fatalf("seedCardDAV: %v", err)
	}
	mb := backend.(*carddav.MockBackend)
	card := mb.CardsByBook["1"]["card1.vcf"]
	if card == nil || !strings.Contains(string(card.CardData), "BEGIN:VCARD") {
		t.Errorf("card1.vcf not seeded from row map: %+v", card)
	}
}

// --- seed.go: seedCalDAV ---

func TestSeedCalDAVBadCalendarObjects(t *testing.T) {
	rec := &Record{Setup: Setup{
		SetupCalDAV:     true,
		CalendarObjects: json.RawMessage(`{not json`),
	}}
	if _, err := seedCalDAV(rec); err == nil {
		t.Fatal("expected error for malformed caldavCalendarObjects")
	}
}

func TestSeedCalDAVSkipsNonClarkKeys(t *testing.T) {
	rec := &Record{Setup: Setup{
		SetupCalDAV: true,
		Calendars:   json.RawMessage(`[{"id":1,"uri":"cal1","not-clark-key":"ignored"}]`),
	}}
	backend, err := seedCalDAV(rec)
	if err != nil {
		t.Fatalf("seedCalDAV: %v", err)
	}
	cals := backend.(*caldav.MockBackend).Calendars
	if len(cals) != 1 || len(cals[0].Props) != 0 {
		t.Errorf("non-clark key should be skipped, got %+v", cals)
	}
}

func TestSeedCalDAVBadPropValues(t *testing.T) {
	tests := []struct {
		name      string
		calendars string
	}{
		{"non-string scalar prop", `[{"id":1,"{DAV:}displayname":42}]`},
		{"unknown __type map", `[{"id":1,"{DAV:}displayname":{"__type":"mystery"}}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &Record{Setup: Setup{
				SetupCalDAV: true,
				Calendars:   json.RawMessage(tc.calendars),
			}}
			if _, err := seedCalDAV(rec); err == nil {
				t.Error("expected error")
			}
		})
	}
}

// --- seed.go: jsonID ---

func TestJsonID(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{"abc", "abc"},
		{float64(42), "42"},
		{true, "true"}, // fallback for odd fixture types
	}
	for _, tc := range tests {
		if got := jsonID(tc.in); got != tc.want {
			t.Errorf("jsonID(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- seed.go: seedPrincipals ---

func TestSeedPrincipalsBadJSON(t *testing.T) {
	if _, err := seedPrincipals(json.RawMessage(`{not json`)); err == nil {
		t.Fatal("expected error for malformed principals fixture")
	}
}

func TestSeedPrincipalsNonClarkProp(t *testing.T) {
	out, err := seedPrincipals(json.RawMessage(`[{"uri":"principals/user1","plainname":"v"}]`))
	if err != nil {
		t.Fatalf("seedPrincipals: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d principals, want 1", len(out))
	}
	if got := out[0].Props[xmlName{Local: "plainname"}]; got != "v" {
		t.Errorf("non-clark prop = %v, want %q in empty namespace", got, "v")
	}
}

// --- seed.go: seedTree / seedNode ---

func TestSeedTreeErrors(t *testing.T) {
	tests := []struct {
		name string
		tree *Tree
	}{
		{"nil tree", nil},
		{"bad root child", &Tree{Type: "roots", Roots: []*Tree{{Type: "bogus"}}}},
		{"unseedable root type", &Tree{Type: "bogus"}},
		{"root not a collection", &Tree{Type: "file", Name: "f.txt"}},
		{"bad synccollection child", &Tree{Type: "synccollection", Name: "s",
			Children: []*Tree{{Type: "bogus"}}}},
		{"bad collection child", &Tree{Type: "collection", Name: "c",
			Children: []*Tree{{Type: "bogus"}}}},
		{"bad propscollection props", &Tree{Type: "propscollection", Name: "p",
			Properties: "not-a-map"}},
		{"bad propscollection prop value", &Tree{Type: "propscollection", Name: "p",
			Properties: map[string]any{"{DAV:}displayname": 42.0}}},
		{"bad propscollection child", &Tree{Type: "propscollection", Name: "p",
			Children: []*Tree{{Type: "bogus"}}}},
		{"CalendarRoot without caldav", &Tree{Type: "node", Class: `Sabre\CalDAV\CalendarRoot`}},
		{"AddressBookRoot without carddav", &Tree{Type: "node", Class: `Sabre\CardDAV\AddressBookRoot`}},
		{"unknown node class", &Tree{Type: "node", Class: `Sabre\Unknown\Thing`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := seedTree(tc.tree, &seedBackends{}); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestSeedNodeStreamingFileWithSize(t *testing.T) {
	size := int64(5)
	node, err := seedNode(&Tree{Type: "streamingfile", Name: "s.txt", Contents: "hello", Size: &size}, &seedBackends{})
	if err != nil {
		t.Fatalf("seedNode: %v", err)
	}
	if node == nil {
		t.Fatal("nil node")
	}
}

// --- seed.go: seedProps ---

func TestSeedProps(t *testing.T) {
	// nil (missing "properties" key) and PHP's []-encoded empty map both
	// mean "no properties".
	for _, raw := range []any{nil, []any{}} {
		props, err := seedProps(raw)
		if err != nil {
			t.Errorf("seedProps(%v): %v", raw, err)
		}
		if props != nil {
			t.Errorf("seedProps(%v) = %v, want nil", raw, props)
		}
	}
	if _, err := seedProps("bogus"); err == nil {
		t.Error("expected error for non-map, non-list fixture")
	}
	if _, err := seedProps(map[string]any{"{DAV:}x": 1.0}); err == nil {
		t.Error("expected error for non-string property value")
	}
}
