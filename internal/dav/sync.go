package dav

import (
	"bytes"
	"context"
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	davxml "calserver/internal/dav/xml"
)

// SyncTokenPrefix is the client-facing token prefix, identical to sabre's
// Sync\Plugin::SYNCTOKEN_PREFIX — existing clients have these stored
// (PLAN.md §5).
const SyncTokenPrefix = "http://sabre.io/ns/sync/"

// ChangeSet is the result of a sync query. Port of the getChanges result
// array.
type ChangeSet struct {
	// SyncToken is the collection's token after these changes (bare, no
	// prefix).
	SyncToken string
	Added     []string
	Modified  []string
	Deleted   []string
	Truncated bool
}

// SyncCollection is a collection supporting RFC 6578 collection sync.
// Port of sabre's ISyncCollection.
type SyncCollection interface {
	Collection
	// SyncToken returns the current token, or "" when sync information is
	// unavailable.
	SyncToken() string
	// Changes returns changes since the given bare token ("" = initial
	// sync). A nil ChangeSet means the token is invalid or unknown.
	Changes(ctx context.Context, syncToken string, syncLevel, limit int) (*ChangeSet, error)
}

// SyncPlugin implements the sync-collection REPORT and sync-token property.
// Port of sabre's Sync\Plugin.
type SyncPlugin struct {
	server *Server
}

func (p *SyncPlugin) PluginName() string { return "sync" }

func (p *SyncPlugin) Init(s *Server) { p.server = s }

// PropFind provides {DAV:}sync-token on sync-capable collections.
func (p *SyncPlugin) PropFind(pf *PropFind, node Node) error {
	pf.Handle(xml.Name{Space: davxml.NSDav, Local: "sync-token"}, func() any {
		if sc, ok := node.(SyncCollection); ok {
			if token := sc.SyncToken(); token != "" {
				return SyncTokenPrefix + token
			}
		}
		return nil
	})
	return nil
}

// SupportedReports advertises sync-collection on sync-capable collections
// with sync info, like sabre's getSupportedReportSet.
func (p *SyncPlugin) SupportedReports(node Node, path string) []xml.Name {
	if sc, ok := node.(SyncCollection); ok && sc.SyncToken() != "" {
		return []xml.Name{{Space: davxml.NSDav, Local: "sync-collection"}}
	}
	return nil
}

// ValidateTokens implements the If-header token validation for sync tokens,
// mirroring sabre's validateTokens listener.
func (p *SyncPlugin) ValidateTokens(ctx context.Context, conditions []IfCondition) {
	for ci := range conditions {
		for ti := range conditions[ci].Tokens {
			token := conditions[ci].Tokens[ti].Token
			if !strings.HasPrefix(token, SyncTokenPrefix) {
				continue
			}
			node, err := p.server.Tree.NodeForPath(ctx, conditions[ci].URI)
			if err != nil {
				continue
			}
			if sc, ok := node.(SyncCollection); ok &&
				sc.SyncToken() == strings.TrimPrefix(token, SyncTokenPrefix) {
				conditions[ci].Tokens[ti].Valid = true
			}
		}
	}
}

type syncCollectionXML struct {
	XMLName   xml.Name `xml:"DAV: sync-collection"`
	SyncToken *struct {
		Value string `xml:",chardata"`
	} `xml:"sync-token"`
	SyncLevel string `xml:"sync-level"`
	Limit     *struct {
		NResults int `xml:"nresults"`
	} `xml:"limit"`
	Prop *struct {
		Names []struct {
			XMLName xml.Name
		} `xml:",any"`
	} `xml:"prop"`
}

// Report implements the {DAV:}sync-collection REPORT.
func (p *SyncPlugin) Report(name xml.Name, body []byte, path string, w http.ResponseWriter, r *http.Request) (bool, error) {
	if (name != xml.Name{Space: davxml.NSDav, Local: "sync-collection"}) {
		return false, nil
	}
	var req syncCollectionXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		return true, ErrBadRequest("%s", err)
	}
	if req.SyncToken == nil {
		return true, ErrBadRequest("The {DAV:}sync-token element is required")
	}
	if req.Prop == nil {
		return true, ErrBadRequest("The {DAV:}prop element is required")
	}

	node, err := p.server.Tree.NodeForPath(r.Context(), path)
	if err != nil {
		return true, err
	}
	sc, isSync := node.(SyncCollection)
	if !isSync {
		return true, davError(http.StatusUnsupportedMediaType, "ReportNotSupported",
			"The {DAV:}sync-collection REPORT is not supported on this url.")
	}
	currentToken := sc.SyncToken()
	if currentToken == "" {
		return true, davError(http.StatusUnsupportedMediaType, "ReportNotSupported",
			"No sync information is available at this node")
	}

	syncToken := strings.TrimSpace(req.SyncToken.Value)
	if syncToken != "" {
		if !strings.HasPrefix(syncToken, SyncTokenPrefix) {
			return true, ErrInvalidSyncToken()
		}
		syncToken = strings.TrimPrefix(syncToken, SyncTokenPrefix)
	}

	syncLevel := 1
	switch {
	case req.SyncLevel == "infinity":
		syncLevel = DepthInfinity
	case req.SyncLevel != "":
		if n, convErr := strconv.Atoi(req.SyncLevel); convErr == nil {
			syncLevel = n
		}
	default:
		// Fall back to the Depth header, like sabre.
		syncLevel = parseDepth(r.Header.Get("Depth"), 1)
	}
	limit := 0
	if req.Limit != nil {
		limit = req.Limit.NResults
	}

	changes, err := sc.Changes(r.Context(), syncToken, syncLevel, limit)
	if err != nil {
		return true, err
	}
	if changes == nil {
		return true, ErrInvalidSyncToken()
	}

	var props []xml.Name
	for _, n := range req.Prop.Names {
		props = append(props, n.XMLName)
	}
	return true, p.writeResponse(r.Context(), w, path, changes, props)
}

func (p *SyncPlugin) writeResponse(ctx context.Context, w http.ResponseWriter, collectionPath string, changes *ChangeSet, props []xml.Name) error {
	ms := &davxml.MultiStatus{SyncToken: SyncTokenPrefix + changes.SyncToken}

	// joinPath avoids "//" hrefs when syncing a root collection.
	joinPath := func(item string) string {
		if collectionPath == "" {
			return item
		}
		return collectionPath + "/" + item
	}

	for _, item := range append(append([]string{}, changes.Added...), changes.Modified...) {
		itemPath := joinPath(item)
		responses, err := p.server.PropertiesForPath(ctx, itemPath, props, 0)
		if err != nil {
			if AsError(err).Status == http.StatusNotFound {
				continue // changed then deleted; skip like sabre's multi-path fetch
			}
			return err
		}
		ms.Responses = append(ms.Responses, responses...)
	}
	for _, item := range changes.Deleted {
		ms.Responses = append(ms.Responses, davxml.Response{
			Href:   p.server.Href(joinPath(item)),
			Status: http.StatusNotFound,
		})
	}
	if changes.Truncated {
		// The single choke point where both backends' truncation surfaces
		// (METRICS-PLAN.md §5); the resource type falls out of the path.
		p.server.Metrics.SyncTruncated(syncResourceLabel(collectionPath))
		href := p.server.BaseURI
		if collectionPath != "" {
			href += encodePath(collectionPath) + "/"
		}
		ms.Responses = append(ms.Responses, davxml.Response{
			Href:   href,
			Status: http.StatusInsufficientStorage,
		})
	}

	return p.server.WriteMultiStatus(w, ms)
}

// syncResourceLabel maps a synced collection's path onto the closed resource
// label set: "calendar", "addressbook", or "other".
func syncResourceLabel(collectionPath string) string {
	switch root, _, _ := strings.Cut(collectionPath, "/"); root {
	case "calendars":
		return "calendar"
	case "addressbooks":
		return "addressbook"
	}
	return "other"
}

// ErrInvalidSyncToken mirrors sabre's Exception\InvalidSyncToken (403 with
// its own exception identity).
func ErrInvalidSyncToken() *Error {
	e := davError(http.StatusForbidden, "InvalidSyncToken", "Invalid or unknown sync token")
	// RFC 6578 §3.2 requires the {DAV:}valid-sync-token precondition in the
	// error body, like sabre.
	e.Precondition = []xml.Name{{Space: davxml.NSDav, Local: "valid-sync-token"}}
	return e
}
