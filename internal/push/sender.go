package push

import (
	"context"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Sender delivers one encrypted Web Push message to a subscription's endpoint
// and reports the push service's HTTP status. It is an interface so the worker
// can be tested without a real push service.
type Sender interface {
	// Send returns the push service's HTTP status code (used to prune dead
	// endpoints on 404/410 and to confirm delivery on 2xx).
	Send(ctx context.Context, sub Subscription, topic string, payload []byte) (status int, err error)
}

// pushTTL is how long the push service should retain an undelivered message
// (30 days), matching the subscription lifetime.
const pushTTL = 30 * 24 * 60 * 60

// WebPushSender implements Sender over RFC 8291 message encryption + RFC 8292
// VAPID via github.com/SherClockHolmes/webpush-go.
type WebPushSender struct{ cfg Config }

// NewWebPushSender builds a sender from the VAPID configuration.
func NewWebPushSender(cfg Config) WebPushSender { return WebPushSender{cfg: cfg} }

func (s WebPushSender) Send(ctx context.Context, sub Subscription, topic string, payload []byte) (int, error) {
	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: sub.PushResource,
		Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.AuthSecret},
	}, &webpush.Options{
		Subscriber:      s.cfg.VAPIDSubject,
		VAPIDPublicKey:  s.cfg.VAPIDPublicKey,
		VAPIDPrivateKey: s.cfg.VAPIDPrivateKey,
		TTL:             pushTTL,
		// The Topic header (RFC 8030 §5.4) collapses an undelivered message
		// with the next one for the same collection. It is capped at 32 chars
		// from the URL-safe alphabet; our base64url topic is longer, so use a
		// stable prefix — unambiguous within a single endpoint, which only
		// ever serves one collection.
		Topic:   topicHeader(topic),
		Urgency: webpush.UrgencyHigh,
	})
	if err != nil {
		return 0, err
	}
	// Drain and close so the underlying connection can be reused.
	resp.Body.Close()
	return resp.StatusCode, nil
}

func topicHeader(topic string) string {
	if len(topic) > 32 {
		return topic[:32]
	}
	return topic
}
