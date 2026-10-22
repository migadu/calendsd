package push

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
)

// registerRequest is the parsed content of a {NS}push-register body: the Web
// Push endpoint and the subscriber's keys.
type registerRequest struct {
	pushResource string
	p256dh       string
	authSecret   string
}

// pushRegisterXML mirrors the WebDAV-Push registration document. Field tags
// carry only local names (no namespace), so a child inherits whichever default
// xmlns the client set — clients emit the whole document under a default
// xmlns="https://bitfire.at/webdav-push", so matching on local name is robust.
type pushRegisterXML struct {
	XMLName      xml.Name `xml:"push-register"`
	Subscription struct {
		WebPush struct {
			PushResource string `xml:"push-resource"`
			PublicKey    string `xml:"subscription-public-key"`
			AuthSecret   string `xml:"auth-secret"`
		} `xml:"web-push-subscription"`
	} `xml:"subscription"`
}

// parseRegister reads a push-register body. It requires the Web Push endpoint
// and both keys; anything missing is a bad request.
func parseRegister(body []byte) (*registerRequest, error) {
	var doc pushRegisterXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&doc); err != nil {
		return nil, err
	}
	wp := doc.Subscription.WebPush
	req := &registerRequest{
		pushResource: strings.TrimSpace(wp.PushResource),
		p256dh:       strings.TrimSpace(wp.PublicKey),
		authSecret:   strings.TrimSpace(wp.AuthSecret),
	}
	if req.pushResource == "" || req.p256dh == "" || req.authSecret == "" {
		return nil, fmt.Errorf("push-register requires web-push-subscription with push-resource, subscription-public-key and auth-secret")
	}
	if err := validatePushResource(req.pushResource); err != nil {
		return nil, err
	}
	return req, nil
}

// buildPushMessage renders the encrypted-payload body a content-update
// notification carries: the collection's opaque topic plus the new sync-token,
// so the receiving client can go straight to a sync-collection REPORT. The
// fragment is self-namespaced (default xmlns) so it does not depend on the
// transport that carries it.
func buildPushMessage(topic, syncToken string) []byte {
	var msg struct {
		XMLName       xml.Name `xml:"https://bitfire.at/webdav-push push-message"`
		Topic         string   `xml:"topic"`
		ContentUpdate struct {
			SyncToken string `xml:"DAV: sync-token"`
		} `xml:"content-update"`
	}
	msg.Topic = topic
	msg.ContentUpdate.SyncToken = syncToken
	return []byte(xml.Header + marshalXML(msg))
}

// webPushTransport renders the web-push transport advertised in the
// transports property, carrying our VAPID public key. It declares its own
// default xmlns so it is self-contained (the marshaller declares NS on the
// multistatus root for the property name but does not rewrite Raw content).
func webPushTransport(vapidPublicKey string) string {
	var t struct {
		XMLName xml.Name `xml:"https://bitfire.at/webdav-push web-push"`
		Key     struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"vapid-public-key"`
	}
	t.Key.Type = "p256ecdsa"
	t.Key.Value = vapidPublicKey
	return marshalXML(t)
}

// marshalXML marshals one of this file's fixed-shape documents, which cannot
// fail.
func marshalXML(v any) string {
	out, err := xml.Marshal(v)
	if err != nil {
		panic("push: marshalling XML: " + err.Error())
	}
	return string(out)
}

// rootElementName returns the qualified name of a document's root element,
// used to tell a push-register POST from any other collection POST.
func rootElementName(body []byte) (xml.Name, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			return xml.Name{}, err
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name, nil
		}
	}
}
