// Package corpus loads and replays the behavioral test corpus extracted from
// sabre/dav's own suite (PLAN.md §7). Records are produced by the
// CorpusRecorder shim in dav/tests/Sabre; regenerate with:
//
//	cd dav && CORPUS_DIR=../testdata/corpus vendor/bin/phpunit \
//	  --configuration tests/phpunit.xml tests/Sabre/CalDAV tests/Sabre/DAV
package corpus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Record is one extracted request/response fixture.
type Record struct {
	// Test is the originating PHP test, e.g. "Sabre\CalDAV\Issue205Test::testIssue205".
	Test string `json:"test"`
	// Seq disambiguates multiple requests within one PHPUnit process.
	Seq   int   `json:"seq"`
	Setup Setup `json:"setup"`
	// File is the record's filename, set by Load.
	File     string   `json:"-"`
	Request  Request  `json:"request"`
	Response Response `json:"response"`
}

// Setup is the server fixture state the test was run against.
type Setup struct {
	SetupCalDAV              bool   `json:"setupCalDAV"`
	SetupCardDAV             bool   `json:"setupCardDAV"`
	SetupACL                 bool   `json:"setupACL"`
	SetupCalDAVSharing       bool   `json:"setupCalDAVSharing"`
	SetupCalDAVScheduling    bool   `json:"setupCalDAVScheduling"`
	SetupCalDAVSubscriptions bool   `json:"setupCalDAVSubscriptions"`
	SetupCalDAVICSExport     bool   `json:"setupCalDAVICSExport"`
	SetupLocks               bool   `json:"setupLocks"`
	SetupFiles               bool   `json:"setupFiles"`
	SetupPropertyStorage     bool   `json:"setupPropertyStorage"`
	AutoLogin                string `json:"autoLogin"`
	// Calendars/CalendarObjects/Principals mirror sabre's mock backend
	// fixtures; kept raw because shapes vary per test (and PHP encodes
	// empty maps as []) — seeding interprets them feature by feature.
	Calendars       json.RawMessage `json:"caldavCalendars"`
	CalendarObjects json.RawMessage `json:"caldavCalendarObjects"`
	Principals      json.RawMessage `json:"principals"`
}

// Request is the captured HTTP request, plus the DAV tree fixture state at
// snapshot time (pre-request).
type Request struct {
	Method  string  `json:"method"`
	URL     string  `json:"url"`
	Headers Headers `json:"headers"`
	Body    string  `json:"body"`
	Tree    *Tree   `json:"tree"`
}

// Tree is a serialized DAV tree fixture from the recorder.
type Tree struct {
	// Type is one of file, collection, propscollection, roots, or node
	// (an arbitrary PHP class the recorder couldn't serialize).
	Type     string `json:"type"`
	Name     string `json:"name"`
	Contents string `json:"contents"`
	// LastModified is a Unix timestamp (sabre mock files default to the
	// extraction time; getlastmodified comparisons must mask it).
	LastModified int64 `json:"lastModified"`
	// FailMode is false or a mode string (PHP's loose typing).
	FailMode any `json:"failMode"`
	// Size is a streamingfile's advertised size; null means unknown.
	Size *int64 `json:"size"`
	// Token/ChangeLog carry a synccollection's sync state.
	Token     int `json:"token"`
	ChangeLog []struct {
		Token    int      `json:"token"`
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Deleted  []string `json:"deleted"`
	} `json:"changeLog"`
	// Properties maps clark-notation names to values; PHP encodes an
	// empty map as [].
	Properties any     `json:"properties"`
	Children   []*Tree `json:"children"`
	Class      string  `json:"class"`
	Roots      []*Tree `json:"roots"`
}

// Response is the reference implementation's response.
type Response struct {
	Status  int     `json:"status"`
	Headers Headers `json:"headers"`
	Body    string  `json:"body"`
}

// Headers tolerates the loose typing of the recorded PHP fixtures: values may
// be strings, numbers, or arrays of either.
type Headers map[string][]string

func (h *Headers) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var anyRaw any
	if err := dec.Decode(&anyRaw); err != nil {
		return err
	}
	// PHP's json_encode turns an empty header map into [].
	raw, ok := anyRaw.(map[string]any)
	if !ok {
		*h = Headers{}
		return nil
	}
	out := make(Headers, len(raw))
	for name, v := range raw {
		vals, ok := v.([]any)
		if !ok {
			vals = []any{v}
		}
		for _, val := range vals {
			out[name] = append(out[name], fmt.Sprint(val))
		}
	}
	*h = out
	return nil
}

// Load reads every corpus record under dir, sorted by filename.
func Load(dir string) ([]Record, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []Record
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var r Record
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		r.File = filepath.Base(p)
		out = append(out, r)
	}
	return out, nil
}

// UsesUnsupportedFeatures reports whether the record depends on fixtures
// outside the MVP port scope (PLAN.md §1); such records stay pending
// indefinitely rather than counting against coverage.
func (r *Record) UsesUnsupportedFeatures() bool {
	s := r.Setup
	return s.SetupCardDAV || s.SetupCalDAVSharing || s.SetupCalDAVScheduling ||
		s.SetupCalDAVSubscriptions || s.SetupLocks
}
