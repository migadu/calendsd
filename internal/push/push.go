// Package push implements WebDAV-Push (draft-bitfire-webdav-push-00): it
// advertises push support on calendar and address-book collections, accepts
// Web Push subscription registrations, and delivers an encrypted Web Push
// notification (RFC 8030/8291, VAPID RFC 8292) whenever a subscribed
// collection's synctoken changes. See PUSH-PLAN.md.
//
// The trigger is a Postgres LISTEN/NOTIFY channel ("dav_push"): the storage
// write path emits pg_notify('dav_push', 'calendar:<id>') inside the same
// transaction that bumps the synctoken, so a committed change — and only a
// committed change — wakes the dispatch worker.
package push

// NS is the WebDAV-Push XML namespace.
const NS = "https://bitfire.at/webdav-push"

// notifyChannel is the Postgres LISTEN/NOTIFY channel the storage write path
// signals on (see internal/storage addChange/abAddChange).
const notifyChannel = "dav_push"

// syncTokenPrefix mirrors sabre's Sync\Plugin::SYNCTOKEN_PREFIX, the form a
// client stores; the push-message carries the changed collection's token in
// this shape so it matches what a following sync-collection REPORT returns.
const syncTokenPrefix = "http://sabre.io/ns/sync/"

// Config carries the VAPID keypair, the topic-derivation secret, and the
// server mount prefix the plugin and worker share.
type Config struct {
	VAPIDSubject    string
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	TopicSecret     string
	// BaseURI is the server mount prefix (begins and ends with "/"), used to
	// build absolute subscription Location URLs.
	BaseURI string
}

// Valid reports whether the secrets required to run push are all present.
// compose() asserts this so an enabled-but-unconfigured server fails closed.
func (c Config) Valid() bool {
	return c.VAPIDPublicKey != "" && c.VAPIDPrivateKey != "" && c.TopicSecret != ""
}

// Subscription is one stored Web Push registration.
type Subscription struct {
	ID             int64
	Token          string // opaque id naming the subscription resource
	Kind           string // "calendar" | "addressbook"
	CollectionID   string // numeric calendars/addressbooks id (as text)
	CollectionPath string // base-relative path, for ACL re-check at dispatch
	PrincipalURI   string
	PushResource   string // Web Push endpoint
	P256dh         string
	AuthSecret     string
	LastSyncToken  int64
}
