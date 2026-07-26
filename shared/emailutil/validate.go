package emailutil

import "strings"

// IsValidEmail performs structural email validation.
// Rejects empty labels (user@.com), consecutive dots (user@domain..com),
// whitespace, control characters, and malformed domains.
func IsValidEmail(addr string) bool {
	// Reject whitespace and control characters (including CRLF to prevent header injection)
	for _, c := range addr {
		if c <= 0x1f || c == 0x7f || c == ' ' {
			return false
		}
	}

	at := strings.LastIndex(addr, "@")
	if at < 1 || at == len(addr)-1 {
		return false
	}
	if strings.Count(addr, "@") != 1 {
		return false
	}

	domain := addr[at+1:]
	if !strings.Contains(domain, ".") {
		return false
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	if strings.Contains(domain, "..") {
		return false
	}
	return true
}
