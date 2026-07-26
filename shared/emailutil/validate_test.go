package emailutil

import "testing"

func TestIsValidEmail(t *testing.T) {
	valid := []string{
		"user@example.com",
		"user+tag@example.com",
		"first.last@example.com",
		"user@mail.example.com",
		"user@example.co.uk",
		"123@example.com",
		"user@my-domain.com",
	}
	for _, addr := range valid {
		if !IsValidEmail(addr) {
			t.Errorf("IsValidEmail(%q) = false, want true", addr)
		}
	}

	invalid := []string{
		"",                     // empty
		"user",                 // no @
		"@example.com",         // empty local part
		"user@",                // empty domain
		"user@.com",            // domain starts with dot (empty label)
		"user@com",             // no dot in domain
		"user@example..com",    // consecutive dots
		"user@example.com.",    // trailing dot
		"user@ example.com",   // space in address
		"user@example\t.com",  // tab
		"user@example\n.com",  // newline
		"user@@example.com",   // double @
		"user@exam\x00ple.com", // null byte
	}
	for _, addr := range invalid {
		if IsValidEmail(addr) {
			t.Errorf("IsValidEmail(%q) = true, want false", addr)
		}
	}
}
