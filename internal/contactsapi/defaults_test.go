package contactsapi

import "testing"

// The DAV listener's 100 MiB body cap used to be passed through to this
// API; a single contact never needs more than a couple of MiB (audit 5.9).
func TestDefaultBodyCap(t *testing.T) {
	if got := (Options{}).withDefaults().MaxBodyBytes; got != 2<<20 {
		t.Errorf("default MaxBodyBytes = %d, want 2 MiB", got)
	}
	if got := (Options{MaxBodyBytes: 4096}).withDefaults().MaxBodyBytes; got != 4096 {
		t.Errorf("explicit MaxBodyBytes overridden: %d", got)
	}
}
