// Object browser and cards page data handlers.

package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"calserver/internal/caldav/ical"
	"calserver/internal/carddav"
	"calserver/internal/storage"
)

// --- Object browser (§3.4) ---

type ObjectsData struct {
	CalendarID string                   `json:"calendarId"`
	Query      string                   `json:"query"`
	Objects    []storage.AdminObjectRow `json:"objects"`
	Truncated  bool                     `json:"truncated"`
}

func (a *App) objectsData(r *http.Request) (any, error) {
	q := r.URL.Query()
	id := q.Get("calendar")
	if id == "" {
		var err error
		if id, err = a.resolveCalendarParam(r.Context(), q.Get("principal"), q.Get("uri")); err != nil {
			return nil, err
		}
	}
	query := storage.AdminObjectQuery{
		CalendarID: id,
		Match:      q.Get("match"),
		InData:     q.Get("indata") == "1",
	}
	var err error
	if query.Start, err = parseTimeParam(q.Get("start")); err != nil {
		return nil, err
	}
	if query.End, err = parseTimeParam(q.Get("end")); err != nil {
		return nil, err
	}
	if lim := q.Get("limit"); lim != "" {
		if query.Limit, err = strconv.Atoi(lim); err != nil {
			return nil, fmt.Errorf("%w: limit: %v", errBadRequest, err)
		}
	}
	objects, truncated, err := a.Store.Objects(r.Context(), query)
	if err != nil {
		return nil, err
	}
	return &ObjectsData{CalendarID: id, Query: query.Match, Objects: objects, Truncated: truncated}, nil
}

func (a *App) resolveCalendarParam(ctx context.Context, principal, uri string) (string, error) {
	if principal == "" || uri == "" {
		return "", fmt.Errorf("%w: need ?calendar= or ?principal=&uri=", errBadRequest)
	}
	id, err := a.Store.LookupCalendar(ctx, principal, uri)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("%w: no calendar %s/%s", errNotFound, principal, uri)
	}
	return id, nil
}

// parseTimeParam accepts RFC 3339 or a raw Unix-seconds integer.
func parseTimeParam(s string) (*int64, error) {
	if s == "" {
		return nil, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return &n, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf("%w: time %q (want RFC 3339 or Unix seconds)", errBadRequest, s)
	}
	n := t.Unix()
	return &n, nil
}

// DerivedFields are the denormalized columns recomputed from calendardata
// with the same ical.Denormalize the write path uses, so a diff means the
// stored row is stale, not that the recomputation differs (§3.4).
type DerivedFields struct {
	ETag            string `json:"etag"`
	Size            int64  `json:"size"`
	ComponentType   string `json:"componentType"`
	UID             string `json:"uid"`
	FirstOccurrence *int64 `json:"firstOccurrence"`
	LastOccurrence  *int64 `json:"lastOccurrence"`
}

type ObjectDetail struct {
	CalendarID string                 `json:"calendarId"`
	Stored     storage.AdminObjectRow `json:"stored"`
	Data       string                 `json:"data"`
	Derived    *DerivedFields         `json:"derived"`
	ParseError string                 `json:"parseError"`
	// Diffs names the stored columns that disagree with the derived
	// values — the "event missing from time-range REPORT" explainer.
	Diffs []string `json:"diffs"`
}

func (a *App) objectData(r *http.Request) (any, error) {
	q := r.URL.Query()
	id, uri := q.Get("calendar"), q.Get("uri")
	if id == "" || uri == "" {
		return nil, fmt.Errorf("%w: need ?calendar=&uri=", errBadRequest)
	}
	obj, err := a.Store.Object(r.Context(), id, uri)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, fmt.Errorf("%w: object %s in calendar %s", errNotFound, uri, id)
	}
	d := &ObjectDetail{CalendarID: id, Stored: obj.Row, Data: string(obj.Data)}
	den, err := ical.Denormalize(obj.Data)
	if err != nil {
		d.ParseError = err.Error()
		return d, nil
	}
	d.Derived = &DerivedFields{
		ETag:            den.ETag,
		Size:            den.Size,
		ComponentType:   den.ComponentType,
		UID:             den.UID,
		FirstOccurrence: den.FirstOccurrence,
		LastOccurrence:  den.LastOccurrence,
	}
	d.Diffs = diffStoredDerived(obj.Row, d.Derived)
	return d, nil
}

func diffStoredDerived(stored storage.AdminObjectRow, derived *DerivedFields) []string {
	var diffs []string
	if stored.ETag != derived.ETag {
		diffs = append(diffs, "etag")
	}
	if stored.Size != derived.Size {
		diffs = append(diffs, "size")
	}
	if stored.ComponentType != derived.ComponentType {
		diffs = append(diffs, "componenttype")
	}
	if stored.UID != derived.UID {
		diffs = append(diffs, "uid")
	}
	if !equalInt64Ptr(stored.FirstOccurrence, derived.FirstOccurrence) {
		diffs = append(diffs, "firstoccurence")
	}
	if !equalInt64Ptr(stored.LastOccurrence, derived.LastOccurrence) {
		diffs = append(diffs, "lastoccurence")
	}
	return diffs
}

func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// --- Cards ---

type CardsData struct {
	BookID    string                 `json:"bookId"`
	Query     string                 `json:"query"`
	Cards     []storage.AdminCardRow `json:"cards"`
	Truncated bool                   `json:"truncated"`
}

func (a *App) cardsData(r *http.Request) (any, error) {
	q := r.URL.Query()
	id := q.Get("book")
	if id == "" {
		return nil, fmt.Errorf("%w: need ?book=", errBadRequest)
	}
	limit := 0
	if lim := q.Get("limit"); lim != "" {
		var err error
		if limit, err = strconv.Atoi(lim); err != nil {
			return nil, fmt.Errorf("%w: limit: %v", errBadRequest, err)
		}
	}
	cards, truncated, err := a.Store.CardsSearch(r.Context(), id, q.Get("match"), q.Get("indata") == "1", limit)
	if err != nil {
		return nil, err
	}
	return &CardsData{BookID: id, Query: q.Get("match"), Cards: cards, Truncated: truncated}, nil
}

type CardDetail struct {
	BookID string               `json:"bookId"`
	Stored storage.AdminCardRow `json:"stored"`
	Data   string               `json:"data"`
	// DerivedETag/DerivedSize follow the cards convention: md5 hex of the
	// payload, payload length.
	DerivedETag string   `json:"derivedEtag"`
	DerivedSize int64    `json:"derivedSize"`
	Diffs       []string `json:"diffs"`
}

func (a *App) cardData(r *http.Request) (any, error) {
	q := r.URL.Query()
	id, uri := q.Get("book"), q.Get("uri")
	if id == "" || uri == "" {
		return nil, fmt.Errorf("%w: need ?book=&uri=", errBadRequest)
	}
	card, err := a.Store.Card(r.Context(), id, uri)
	if err != nil {
		return nil, err
	}
	if card == nil {
		return nil, fmt.Errorf("%w: card %s in book %s", errNotFound, uri, id)
	}
	d := &CardDetail{BookID: id, Stored: card.Row, Data: string(card.Data)}
	d.DerivedETag = strings.Trim((&carddav.CardInfo{CardData: card.Data}).QuotedETag(), `"`)
	d.DerivedSize = int64(len(card.Data))
	if d.Stored.ETag != d.DerivedETag {
		d.Diffs = append(d.Diffs, "etag")
	}
	if d.Stored.Size != d.DerivedSize {
		d.Diffs = append(d.Diffs, "size")
	}
	return d, nil
}
