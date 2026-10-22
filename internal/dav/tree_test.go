package dav

import "testing"

// TestCopyMoveIntoSelfForbidden guards against the infinite-recursion OOM found
// by CalDAVTester's CardDAV/errorcondition.xml: COPY / with Destination /copy/
// copied the root collection into a child of itself, and copyNode kept
// re-reading the child list it was still writing to until the process was
// OOM-killed. The copy/move must be rejected with 403 before touching nodes.
func TestCopyMoveIntoSelfForbidden(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src, dst string
	}{
		{"copy root into child", "", "copy"},
		{"collection into descendant", "calendars", "calendars/sub"},
		{"collection onto itself", "calendars", "calendars"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, op := range []struct {
				label string
				run   func() error
			}{
				{"Copy", func() error { return NewTree(testRoot()).Copy(t.Context(), tc.src, tc.dst, DepthInfinity) }},
				{"Move", func() error { return NewTree(testRoot()).Move(t.Context(), tc.src, tc.dst) }},
			} {
				de, ok := op.run().(*Error)
				if !ok || de.Status != 403 {
					t.Fatalf("%s(%q,%q) = %v, want 403 Forbidden", op.label, tc.src, tc.dst, de)
				}
			}
		})
	}

	// A copy to a sibling must not trip the guard (it fails later only because
	// testRoot has no children, i.e. not with 403).
	if de, ok := NewTree(testRoot()).Copy(t.Context(), "calendars", "backup", DepthInfinity).(*Error); ok && de.Status == 403 {
		t.Fatalf("Copy to sibling wrongly forbidden: %v", de)
	}
}

func TestPathContainsOrEquals(t *testing.T) {
	for _, tc := range []struct {
		ancestor, descendant string
		want                 bool
	}{
		{"", "anything", true},
		{"a", "a", true},
		{"a", "a/b", true},
		{"a", "ab", false},
		{"a/b", "a/c", false},
		{"/calendars/", "calendars/x", true},
	} {
		if got := pathContainsOrEquals(tc.ancestor, tc.descendant); got != tc.want {
			t.Errorf("pathContainsOrEquals(%q,%q)=%v want %v",
				tc.ancestor, tc.descendant, got, tc.want)
		}
	}
}
