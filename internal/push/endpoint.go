package push

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// sendTimeout bounds one delivery to a push service.
const sendTimeout = 30 * time.Second

// cgnat is RFC 6598 shared address space, which net/netip does not class as
// private but which is just as internal.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// publicAddr reports whether a push service may live at addr: anything but
// loopback, private, link-local, CGNAT, multicast or unspecified addresses,
// so a subscriber cannot aim the server's requests at its own network.
func publicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr.IsValid() && addr.IsGlobalUnicast() && !addr.IsPrivate() && !cgnat.Contains(addr)
}

// validatePushResource checks a subscriber-supplied endpoint before it is
// stored: https only, no credentials, and not a literal internal address or
// localhost. Hostnames are checked again at dial time (pushClient), after
// resolution, so DNS cannot be used to get around this.
func validatePushResource(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("push-resource must be an https URL")
	}
	host := u.Hostname()
	if addr, err := netip.ParseAddr(host); err == nil && !publicAddr(addr) {
		return fmt.Errorf("push-resource must not point at an internal address")
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return fmt.Errorf("push-resource must not point at an internal address")
	}
	return nil
}

// pushClient delivers to push services. It refuses to connect to internal
// addresses (checked on the resolved address, per connection), and neither
// follows redirects nor uses an environment proxy, either of which would
// bypass that check.
var pushClient = &http.Client{
	Timeout: sendTimeout,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				ap, err := netip.ParseAddrPort(address)
				if err != nil || !publicAddr(ap.Addr()) {
					return fmt.Errorf("push: refusing to connect to internal address %s", address)
				}
				return nil
			},
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}
