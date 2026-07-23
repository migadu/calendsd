package corpus

import (
	"net/http/httptest"
	"strings"
	"testing"

	"calserver/internal/dav"
	davxml "calserver/internal/dav/xml"
)

// passing lists the corpus records the Go server must replay correctly.
// Porting a feature moves its records from pending into this set; CI fails if
// a listed record regresses. This is the port's progress metric.
var passing = map[string]bool{
	// Phase 0: no tree, no backends — nothing replays yet.
}

// TestCorpus replays every extracted sabre record against the Go server.
// Records in the passing set must match the reference response; the rest are
// reported as pending (the red→green driver of the port).
func TestCorpus(t *testing.T) {
	records, err := Load("../../testdata/corpus")
	if err != nil {
		t.Fatalf("loading corpus: %v", err)
	}
	if len(records) == 0 {
		t.Skip("no corpus records extracted; run the CorpusRecorder (see package doc)")
	}

	var green, pending, outOfScope int
	for _, rec := range records {
		switch {
		case rec.UsesUnsupportedFeatures():
			outOfScope++
		case passing[rec.File]:
			green++
			rec := rec
			t.Run(rec.File, func(t *testing.T) { replay(t, &rec) })
		default:
			pending++
		}
	}
	t.Logf("corpus coverage: %d green, %d pending, %d out of MVP scope (of %d records)",
		green, pending, outOfScope, len(records))
}

// replay seeds a server from the record's setup, replays the request, and
// compares the response semantically.
func replay(t *testing.T, rec *Record) {
	t.Helper()
	// Backends land in later phases; the server grows as records move to
	// the passing set.
	srv := dav.NewServer("/")

	req := httptest.NewRequest(rec.Request.Method, orSlash(rec.Request.URL), strings.NewReader(rec.Request.Body))
	for name, vals := range rec.Request.Headers {
		for _, v := range vals {
			req.Header.Add(name, v)
		}
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != rec.Response.Status {
		t.Errorf("status = %d, sabre = %d; body:\n%s", w.Code, rec.Response.Status, w.Body.String())
	}
	compareBodies(t, w.Body.String(), rec.Response.Body)
}

func orSlash(u string) string {
	if u == "" {
		return "/"
	}
	return u
}

// compareBodies compares semantically: XML bodies via the canonicalizer,
// anything else byte-exact.
func compareBodies(t *testing.T, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	if looksLikeXML(got) && looksLikeXML(want) {
		cGot, errG := davxml.Canonicalize(strings.NewReader(got))
		cWant, errW := davxml.Canonicalize(strings.NewReader(want))
		if errG == nil && errW == nil {
			if cGot != cWant {
				t.Errorf("XML bodies differ semantically:\n--- ours ---\n%s\n--- sabre ---\n%s", cGot, cWant)
			}
			return
		}
	}
	t.Errorf("bodies differ:\n--- ours ---\n%s\n--- sabre ---\n%s", got, want)
}

func looksLikeXML(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "<?xml") || strings.HasPrefix(s, "<")
}
