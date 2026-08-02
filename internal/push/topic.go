package push

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// Topic derives the opaque, server-wide-unique, stable push topic for a
// collection. It is HMAC-SHA256(topic_secret, "kind:id") so it is
// deterministic (the advertised topic and the worker's derived topic can't
// drift), non-enumerable (an attacker can't guess another collection's topic
// without the secret), and needs no schema column. kind is "calendar" or
// "addressbook"; id is the numeric collection id as text — the same
// "kind:id" the pg_notify payload carries.
func (c Config) Topic(kind, id string) string {
	mac := hmac.New(sha256.New, []byte(c.TopicSecret))
	fmt.Fprintf(mac, "%s:%s", kind, id)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
