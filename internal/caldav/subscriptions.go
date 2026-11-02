package caldav

import (
	"context"
	"encoding/xml"

	davxml "github.com/migadu/calendsd/internal/dav/xml"
)

// SubscriptionInfo is one calendar subscription (a server-stored reference
// to an external ICS feed).
type SubscriptionInfo struct {
	ID           string
	URI          string
	PrincipalURI string
	// Props holds the subscription properties ({cs}source, displayname,
	// refreshrate, strip* flags, ...).
	Props map[xml.Name]any
}

// SubscriptionsBackend stores subscriptions (the calendarsubscriptions
// table). Port of sabre's Backend\SubscriptionSupport.
type SubscriptionsBackend interface {
	SubscriptionsForUser(ctx context.Context, principalURI string) ([]SubscriptionInfo, error)
	CreateSubscription(ctx context.Context, principalURI, uri string, props map[xml.Name]any) (string, error)
	DeleteSubscription(ctx context.Context, id string) error
}

// nameSubscribed is the resourcetype marking a subscription collection.
var nameSubscribed = xml.Name{Space: davxml.NSCalendarServer, Local: "subscribed"}

// MockSubscriptionBackend ports sabre's Backend\MockSubscriptionSupport.
type MockSubscriptionBackend struct {
	MockBackend
	Subscriptions []SubscriptionInfo
	nextSubID     int
}

func (m *MockSubscriptionBackend) SubscriptionsForUser(ctx context.Context, principalURI string) ([]SubscriptionInfo, error) {
	var out []SubscriptionInfo
	for _, s := range m.Subscriptions {
		if s.PrincipalURI == principalURI {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *MockSubscriptionBackend) CreateSubscription(ctx context.Context, principalURI, uri string, props map[xml.Name]any) (string, error) {
	m.nextSubID++
	id := string(rune('0' + m.nextSubID))
	m.Subscriptions = append(m.Subscriptions, SubscriptionInfo{
		ID: id, URI: uri, PrincipalURI: principalURI, Props: props,
	})
	return id, nil
}

func (m *MockSubscriptionBackend) DeleteSubscription(ctx context.Context, id string) error {
	for i, s := range m.Subscriptions {
		if s.ID == id {
			m.Subscriptions = append(m.Subscriptions[:i], m.Subscriptions[i+1:]...)
			return nil
		}
	}
	return nil
}
