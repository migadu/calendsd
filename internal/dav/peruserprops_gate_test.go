package dav

// Audit 5.2: the caldav ACL grants read-only delegates write-properties so
// their per-user writes get through, but the per-user plugin only claimed
// the whitelisted names, so a read delegate's PROPPATCH of displayname fell
// through and renamed the owner's calendar.

import (
	"context"
	"encoding/xml"
	"net/http"
	"testing"
)

func TestReadOnlyDelegateCannotWriteSharedProps(t *testing.T) {
	color := xml.Name{Space: "http://apple.com/ns/ical/", Local: "calendar-color"}
	displayname := xml.Name{Space: "DAV:", Local: "displayname"}
	node := ownedColl{owner: "principals/user1"}
	const path = "calendars/user1/cal"

	for _, tc := range []struct {
		name     string
		gate     func(context.Context, string) bool
		wantCode int // for displayname
	}{
		{"read-only delegate", func(context.Context, string) bool { return false }, http.StatusForbidden},
		{"nil gate fails closed", nil, http.StatusForbidden},
		{"write delegate", func(context.Context, string) bool { return true }, 0},
	} {
		store := &memPropStore{}
		plugin := &PerUserPropsPlugin{Backend: store, Names: []xml.Name{color}, MayWriteShared: tc.gate}
		pp := NewPropPatch(map[xml.Name]any{color: "#abcdef", displayname: "Renamed"})
		if err := plugin.PrePropPatch(perUserCtx("principals/user2"), path, node, pp); err != nil {
			t.Fatal(err)
		}
		remaining := pp.Remaining()
		if tc.wantCode == 0 {
			if len(remaining) != 1 || remaining[0] != displayname {
				t.Errorf("%s: displayname should fall through to shared handling, remaining = %v", tc.name, remaining)
			}
			continue
		}
		if len(remaining) != 0 {
			t.Errorf("%s: shared property left unclaimed (would reach the owner's row): %v", tc.name, remaining)
		}
		result, ok := pp.Commit()
		if ok {
			t.Errorf("%s: commit succeeded; want the 403 to fail it", tc.name)
		}
		if result[displayname] != http.StatusForbidden {
			t.Errorf("%s: displayname status = %d, want 403 (result %v)", tc.name, result[displayname], result)
		}
		if _, shared := store.props[path][displayname]; shared {
			t.Errorf("%s: displayname landed in shared storage", tc.name)
		}
		// All-or-nothing: the per-user colour must not persist either, and
		// it reports 424, not 200.
		if _, persisted := store.props[path+"/\nprincipals/user2"][color]; persisted {
			t.Errorf("%s: per-user colour persisted inside a failed PROPPATCH", tc.name)
		}
		if result[color] != http.StatusFailedDependency {
			t.Errorf("%s: colour status = %d, want 424", tc.name, result[color])
		}
	}
}
