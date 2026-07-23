package ical

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// goldenRecord mirrors one entry of testdata/golden.json, generated from
// sabre's own getDenormalizedData() by scripts/denormalize-golden.php.
// Regenerate after changing fixtures:
//
//	php scripts/denormalize-golden.php internal/caldav/ical/testdata/*.ics \
//	  > internal/caldav/ical/testdata/golden.json
type goldenRecord struct {
	ETag            string `json:"etag"`
	Size            int64  `json:"size"`
	ComponentType   string `json:"componentType"`
	FirstOccurrence *int64 `json:"firstOccurence"`
	LastOccurrence  *int64 `json:"lastOccurence"`
	UID             string `json:"uid"`
}

// TestDenormalizeMatchesSabre is the phase-0 parity gate: the Go
// denormalization must agree with the reference PHP implementation on every
// fixture, field for field.
func TestDenormalizeMatchesSabre(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "golden.json"))
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	var golden map[string]goldenRecord
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parsing golden file: %v", err)
	}
	if len(golden) == 0 {
		t.Fatal("golden file is empty")
	}

	for name, want := range golden {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatalf("reading fixture: %v", err)
			}
			got, err := Denormalize(data)
			if err != nil {
				t.Fatalf("Denormalize: %v", err)
			}
			if got.ETag != want.ETag {
				t.Errorf("ETag = %s, sabre = %s", got.ETag, want.ETag)
			}
			if got.Size != want.Size {
				t.Errorf("Size = %d, sabre = %d", got.Size, want.Size)
			}
			if got.ComponentType != want.ComponentType {
				t.Errorf("ComponentType = %s, sabre = %s", got.ComponentType, want.ComponentType)
			}
			if got.UID != want.UID {
				t.Errorf("UID = %s, sabre = %s", got.UID, want.UID)
			}
			checkOccurrence(t, "FirstOccurrence", got.FirstOccurrence, want.FirstOccurrence)
			checkOccurrence(t, "LastOccurrence", got.LastOccurrence, want.LastOccurrence)
		})
	}
}

func checkOccurrence(t *testing.T, field string, got, want *int64) {
	t.Helper()
	switch {
	case (got == nil) != (want == nil):
		t.Errorf("%s = %v, sabre = %v", field, fmtPtr(got), fmtPtr(want))
	case got != nil && *got != *want:
		t.Errorf("%s = %d, sabre = %d (delta %+d s)", field, *got, *want, *got-*want)
	}
}

func fmtPtr(p *int64) any {
	if p == nil {
		return "nil"
	}
	return *p
}
