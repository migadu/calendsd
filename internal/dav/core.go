package dav

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	davxml "calserver/internal/dav/xml"
)

// CorePlugin implements the baseline WebDAV protocol behavior, mirroring
// sabre's CorePlugin.
type CorePlugin struct {
	server *Server
}

func (c *CorePlugin) PluginName() string { return "core" }

func (c *CorePlugin) Init(s *Server) { c.server = s }

// Features implements FeatureProvider. "1" and "3" are WebDAV compliance
// classes (RFC 4918); extended-mkcol is RFC 5689. Class "2" (locks) is
// deliberately absent — locks are out of MVP scope.
func (c *CorePlugin) Features() []string {
	return []string{"1", "3", "extended-mkcol"}
}

// Methods implements MethodProvider. Matches sabre's getAllowedMethods:
// MKCOL is only advertised on unmapped URIs.
func (c *CorePlugin) Methods(path string) []string {
	methods := []string{
		"OPTIONS", "GET", "HEAD", "DELETE", "PROPFIND",
		"PUT", "PROPPATCH", "COPY", "MOVE", "REPORT",
	}
	if c.server.Tree == nil || !c.server.Tree.NodeExists(path) {
		methods = append(methods, "MKCOL")
	}
	return methods
}

func (c *CorePlugin) HandleMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	var err error
	switch r.Method {
	case "OPTIONS":
		err = c.options(w, r)
	case "GET":
		return c.get(w, r, false)
	case "HEAD":
		err = c.head(w, r)
	case "DELETE":
		err = c.delete(w, r)
	case "PROPFIND":
		err = c.propfind(w, r)
	case "PROPPATCH":
		err = c.proppatch(w, r)
	case "PUT":
		err = c.put(w, r)
	case "MKCOL":
		err = c.mkcol(w, r)
	case "COPY":
		err = c.copy(w, r)
	case "MOVE":
		err = c.move(w, r)
	case "REPORT":
		err = c.report(w, r)
	default:
		return false, nil
	}
	return true, err
}

// report parses the report's root element name and dispatches to plugins,
// like sabre's httpReport + the report event.
func (c *CorePlugin) report(w http.ResponseWriter, r *http.Request) error {
	path, err := c.server.Path(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	name, err := rootElementName(body)
	if err != nil {
		return ErrBadRequest("%s", err)
	}
	c.server.Metrics.ReportRequest(reportTypeLabel(name))
	for _, p := range c.server.Plugins() {
		rh, ok := p.(ReportHandler)
		if !ok {
			continue
		}
		handled, err := rh.Report(name, body, path, w, r)
		if err != nil {
			return err
		}
		if handled {
			return nil
		}
	}
	return ErrReportNotSupported()
}

// reportTypeLabel maps a REPORT root element onto the closed metric label
// set; unknown reports collapse to "other" so the label never grows from
// request data.
func reportTypeLabel(name xml.Name) string {
	const (
		nsCalDAV  = "urn:ietf:params:xml:ns:caldav"
		nsCardDAV = "urn:ietf:params:xml:ns:carddav"
	)
	switch name {
	case xml.Name{Space: nsCalDAV, Local: "calendar-query"},
		xml.Name{Space: nsCalDAV, Local: "calendar-multiget"},
		xml.Name{Space: nsCalDAV, Local: "free-busy-query"},
		xml.Name{Space: nsCardDAV, Local: "addressbook-query"},
		xml.Name{Space: nsCardDAV, Local: "addressbook-multiget"}:
		return name.Local
	case xml.Name{Space: davxml.NSDav, Local: "sync-collection"}:
		return "sync-collection"
	}
	return "other"
}

// rootElementName returns the qualified name of a document's root element.
func rootElementName(body []byte) (xml.Name, error) {
	dec := xml.NewDecoder(strings.NewReader(string(body)))
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

// options answers OPTIONS with the DAV and Allow headers.
func (c *CorePlugin) options(w http.ResponseWriter, r *http.Request) error {
	// Resolve to the server-relative path (BaseURI stripped, decoded) so
	// MKCOL is only advertised on genuinely unmapped URIs, like sabre.
	path, err := c.server.Path(r)
	if err != nil {
		path = ""
	}
	h := w.Header()
	h.Set("DAV", strings.Join(c.server.Features(), ", "))
	h.Set("Allow", strings.Join(c.server.AllowedMethods(path), ", "))
	h.Set("MS-Author-Via", "DAV")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
	return nil
}

// get serves file bodies with Range support. With headOnly the body is
// omitted (sabre's X-Sabre-Original-Method mechanism). handled=false means
// the node is not a file, so another plugin may serve it.
func (c *CorePlugin) get(w http.ResponseWriter, r *http.Request, headOnly bool) (handled bool, err error) {
	path, err := c.server.Path(r)
	if err != nil {
		return true, err
	}
	node, err := c.server.Tree.NodeForPath(path)
	if err != nil {
		return true, err
	}

	// Plugins may take over serving (content-type transforms and
	// non-file nodes like notifications; sabre's afterMethod:GET
	// equivalents). Consulted before the File check so a plugin can
	// serve a node that is not an IFile.
	if !headOnly {
		for _, p := range c.server.Plugins() {
			if hook, ok := p.(GetHandlerHook); ok {
				handled, err := hook.HandleGet(node, w, r)
				if handled || err != nil {
					return true, err
				}
			}
		}
	}

	file, isFile := node.(File)
	if !isFile {
		return false, nil
	}

	h := w.Header()
	contentType := file.ContentType()
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	h.Set("Content-Type", contentType)
	// Calendar/contact objects are authenticated and change in place under a
	// stable URL, so a cache that serves a stale body also serves a stale ETag
	// — which a client then replays as a doomed If-Match, failing its next PUT
	// with a 412. "private, no-cache" lets a cache store the response but forces
	// revalidation (a conditional GET, still 304-efficient) before every reuse,
	// and keeps shared caches from holding one user's data. "no-transform" tells
	// a reverse proxy not to gzip the body: compression rewrites the strong ETag
	// into a weak one (RFC 7232 §2.3.1), which the editor then replays as a
	// weak If-Match that would fail its next conditional PUT (the server
	// tolerates that in normalizeETag, but not transforming is cleaner).
	h.Set("Cache-Control", "private, no-cache, no-transform")
	if etag := file.ETag(); etag != "" {
		h.Set("ETag", etag)
	}
	if lm := file.LastModified(); !lm.IsZero() {
		h.Set("Last-Modified", lm.UTC().Format(http.TimeFormat))
	}

	// A negative size means unknown (streaming nodes): no Content-Length
	// and no range support, like sabre with a null getSize().
	size := file.Size()
	var hasRange bool
	var start, end int64
	if size >= 0 {
		start, end, hasRange, err = parseRange(r.Header.Get("Range"), size)
		if err != nil {
			return true, err
		}
		if hasRange && ignoreRange(r.Header.Get("If-Range"), h) {
			hasRange = false
		}
	}

	if headOnly {
		if size >= 0 {
			h.Set("Content-Length", strconv.FormatInt(size, 10))
		}
		w.WriteHeader(http.StatusOK)
		return true, nil
	}

	body, err := file.Get()
	if err != nil {
		return true, err
	}
	defer body.Close()

	if size < 0 {
		w.WriteHeader(http.StatusOK)
		_, err = io.Copy(w, body)
		return true, err
	}

	if hasRange {
		h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
		if _, err := io.CopyN(io.Discard, body, start); err != nil {
			return true, err
		}
		_, err = io.CopyN(w, body, end-start+1)
		return true, err
	}
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, err = io.Copy(w, body)
	return true, err
}

// parseRange interprets a single-range Range header against the entity size,
// mirroring sabre's getHTTPRange + httpGet checks (including 416 semantics).
func parseRange(header string, size int64) (start, end int64, ok bool, err error) {
	if header == "" || size == 0 {
		return 0, 0, false, nil
	}
	var s, e string
	if n, _ := fmt.Sscanf(header, "bytes=%s", &s); n != 1 {
		return 0, 0, false, nil
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, nil
	}
	s, e = parts[0], parts[1]
	switch {
	case s != "":
		start, err = strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, 0, false, nil
		}
		end = size - 1
		if e != "" {
			end, err = strconv.ParseInt(e, 10, 64)
			if err != nil {
				return 0, 0, false, nil
			}
		}
		if start >= size {
			return 0, 0, false, davError(http.StatusRequestedRangeNotSatisfiable,
				"RequestedRangeNotSatisfiable",
				"The start offset (%d) exceeded the size of the entity (%d)", start, size)
		}
		if end < start {
			return 0, 0, false, davError(http.StatusRequestedRangeNotSatisfiable,
				"RequestedRangeNotSatisfiable",
				"The end offset (%s) is lower than the start offset (%s)", e, s)
		}
		if end >= size {
			end = size - 1
		}
	case e != "":
		var suffix int64
		suffix, err = strconv.ParseInt(e, 10, 64)
		if err != nil {
			return 0, 0, false, nil
		}
		start = size - suffix
		if start < 0 {
			start = 0
		}
		end = size - 1
	default:
		return 0, 0, false, nil
	}
	return start, end, true, nil
}

// ignoreRange implements If-Range: serve the full entity when the validator
// doesn't match. h carries the already-computed ETag/Last-Modified.
func ignoreRange(ifRange string, h http.Header) bool {
	if ifRange == "" {
		return false
	}
	if strings.HasPrefix(ifRange, `"`) || strings.HasPrefix(ifRange, `W/`) {
		return h.Get("ETag") == "" || h.Get("ETag") != ifRange
	}
	lm := h.Get("Last-Modified")
	if lm == "" {
		return true
	}
	modified, err1 := http.ParseTime(lm)
	limit, err2 := http.ParseTime(ifRange)
	return err1 != nil || err2 != nil || modified.After(limit)
}

// head runs GET without a body; 501 (no handler) converts to an empty 200
// for client compatibility, like sabre.
func (c *CorePlugin) head(w http.ResponseWriter, r *http.Request) error {
	handled, err := c.get(w, r, true)
	if err != nil {
		return err
	}
	if !handled {
		h := w.Header()
		h.Set("Content-Type", "text/plain")
		h.Set("X-Sabre-Real-Status", "501")
		w.WriteHeader(http.StatusOK)
	}
	return nil
}

func (c *CorePlugin) delete(w http.ResponseWriter, r *http.Request) error {
	path, err := c.server.Path(r)
	if err != nil {
		return err
	}
	for _, p := range c.server.Plugins() {
		if hook, ok := p.(BeforeUnbindHook); ok {
			if err := hook.BeforeUnbind(r, path); err != nil {
				return err
			}
		}
	}
	if err := c.server.Tree.Delete(path); err != nil {
		return err
	}
	c.server.fireAfterUnbind(path)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (c *CorePlugin) propfind(w http.ResponseWriter, r *http.Request) error {
	path, err := c.server.Path(r)
	if err != nil {
		return err
	}
	pf, err := davxml.ParsePropfind(r.Body)
	if err != nil {
		return ErrBadRequest("%s", err)
	}
	depth := parseDepth(r.Header.Get("Depth"), 1)

	var names []xml.Name
	if pf.Type == davxml.PropfindProp {
		names = pf.Props
	}
	// allprop with <include> still requests the included properties
	// (RFC 4918 §9.1) without turning off allprop semantics.
	responses, err := c.server.PropertiesForPath(r.Context(), path, names, depth, pf.Include...)
	if err != nil {
		return err
	}
	switch {
	case pf.Type == davxml.PropfindPropName:
		// propname: return only the names of the properties that exist,
		// with empty values and status 200 (RFC 4918 §9.1).
		for i := range responses {
			responses[i].Propstats = propNamesOnly(responses[i].Propstats)
		}
	case preferMinimal(r):
		for i := range responses {
			responses[i].Propstats = stripStatus(responses[i].Propstats, http.StatusNotFound)
		}
	}

	h := w.Header()
	h.Set("Content-Type", "application/xml; charset=utf-8")
	h.Set("Vary", "Brief,Prefer")
	// iCal depends on DAV headers on PROPFIND responses, like sabre notes.
	h.Set("DAV", strings.Join(c.server.Features(), ", "))
	body, err := (&davxml.MultiStatus{Responses: responses}).Marshal()
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusMultiStatus)
	_, err = w.Write(body)
	return err
}

func (c *CorePlugin) proppatch(w http.ResponseWriter, r *http.Request) error {
	path, err := c.server.Path(r)
	if err != nil {
		return err
	}
	update, err := davxml.ParsePropertyUpdate(r.Body)
	if err != nil {
		return ErrBadRequest("%s", err)
	}
	result, err := c.server.UpdateProperties(r.Context(), path, update)
	if err != nil {
		return err
	}

	if preferMinimal(r) {
		ok := true
		for _, code := range result {
			if code > 299 {
				ok = false
			}
		}
		if ok {
			w.Header().Set("Vary", "Brief,Prefer")
			w.WriteHeader(http.StatusNoContent)
			return nil
		}
	}

	resp := propstatResponse(c.server.Href(path), update.Names, result)

	h := w.Header()
	h.Set("Content-Type", "application/xml; charset=utf-8")
	h.Set("Vary", "Brief,Prefer")
	body, err := (&davxml.MultiStatus{Responses: []davxml.Response{resp}}).Marshal()
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusMultiStatus)
	_, err = w.Write(body)
	return err
}

// propstatResponse renders per-property results as one response with one
// propstat per status code, statuses and properties in the order of names.
func propstatResponse(href string, names []xml.Name, result map[xml.Name]int) davxml.Response {
	byStatus := map[int][]davxml.Property{}
	var statuses []int
	for _, n := range names {
		status := result[n]
		if _, seen := byStatus[status]; !seen {
			statuses = append(statuses, status)
		}
		byStatus[status] = append(byStatus[status], davxml.Property{Name: n})
	}
	resp := davxml.Response{Href: href}
	for _, s := range statuses {
		resp.Propstats = append(resp.Propstats, davxml.Propstat{Status: s, Props: byStatus[s]})
	}
	return resp
}

func (c *CorePlugin) put(w http.ResponseWriter, r *http.Request) error {
	path, err := c.server.Path(r)
	if err != nil {
		return err
	}
	if r.Header.Get("Content-Range") != "" {
		return ErrBadRequest("Content-Range on PUT requests are forbidden.")
	}

	body := io.Reader(r.Body)
	// Finder's chunked PUTs can arrive empty; sabre fails them to protect
	// the user (X-Expected-Entity-Length announces the intended size).
	if expected := r.Header.Get("X-Expected-Entity-Length"); expected != "" && expected != "0" {
		first := make([]byte, 1)
		n, _ := io.ReadFull(r.Body, first)
		if n != 1 {
			return ErrForbidden("This server is not compatible with OS/X finder. Consider using a different WebDAV client or webserver.")
		}
		body = io.MultiReader(strings.NewReader(string(first[:n])), r.Body)
	}

	isNew := !c.server.Tree.NodeExists(path)

	// Write interceptors (the scheduling broker) may rewrite the payload.
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	sent := raw
	for _, p := range c.server.Plugins() {
		if hook, ok := p.(WriteInterceptor); ok {
			raw, err = hook.InterceptWrite(r, path, raw, isNew)
			if err != nil {
				return err
			}
		}
	}
	body = bytes.NewReader(raw)
	// RFC 4791 §5.3.4: a server that stores something other than the body it
	// was sent must not return a strong ETag for it — the client would take
	// the tag to describe its own bytes. (sabre does the same.)
	modified := !bytes.Equal(sent, raw)

	if !isNew {
		node, err := c.server.Tree.NodeForPath(path)
		if err != nil {
			return err
		}
		file, isFile := node.(File)
		if !isFile {
			return ErrConflict("PUT is not allowed on non-files.")
		}
		etag, err := file.Put(body)
		if err != nil {
			return err
		}
		c.fireAfterWrite(r, path)
		if modified {
			etag = ""
		}
		writeCreated(w, etag, http.StatusNoContent)
		return nil
	}

	parentPath, name := splitPath(path)
	if !c.server.Tree.NodeExists(parentPath) {
		return ErrConflict("Files can not be created in non-existent collections")
	}
	parent, err := c.server.Tree.NodeForPath(parentPath)
	if err != nil {
		return err
	}
	coll, isColl := parent.(Collection)
	if !isColl {
		return ErrConflict("Files can only be created as children of collections")
	}
	etag, err := coll.CreateFile(name, body)
	if err != nil {
		return err
	}
	c.fireAfterWrite(r, path)
	if modified {
		etag = ""
	}
	writeCreated(w, etag, http.StatusCreated)
	return nil
}

// fireAfterWrite notifies plugins that a PUT has been persisted (sabre's
// afterWriteContent/afterCreateFile events).
func (c *CorePlugin) fireAfterWrite(r *http.Request, path string) {
	for _, p := range c.server.Plugins() {
		if hook, ok := p.(AfterWriteHook); ok {
			hook.AfterWrite(r, path)
		}
	}
}

func writeCreated(w http.ResponseWriter, etag string, status int) {
	w.Header().Set("Content-Length", "0")
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(status)
}

func (c *CorePlugin) mkcol(w http.ResponseWriter, r *http.Request) error {
	path, err := c.server.Path(r)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	var extraProps map[xml.Name]any
	var resourceTypes []xml.Name
	if len(body) > 0 {
		contentType := r.Header.Get("Content-Type")
		if !strings.HasPrefix(contentType, "application/xml") && !strings.HasPrefix(contentType, "text/xml") {
			return ErrUnsupportedMediaType("The request body for the MKCOL request must have an xml Content-Type")
		}
		resourceType, props, err := davxml.ParseMkcol(body)
		if err != nil {
			return ErrBadRequest("%s", err)
		}
		if !slices.Contains(resourceType, xml.Name{Space: davxml.NSDav, Local: "collection"}) {
			return ErrForbidden("Unable to create a collection of this type")
		}
		extraProps = props
		resourceTypes = resourceType
	}

	if c.server.Tree.NodeExists(path) {
		return ErrMethodNotAllowed("The resource you tried to create already exists")
	}
	parentPath, name := splitPath(path)
	parent, err := c.server.Tree.NodeForPath(parentPath)
	if err != nil {
		if AsError(err).Status == http.StatusNotFound {
			return ErrConflict("Parent node does not exist")
		}
		return err
	}
	coll, isColl := parent.(Collection)
	if !isColl {
		return ErrConflict("Parent node is not a collection")
	}
	// Non-plain resource types go to extended-collection parents
	// (calendars, subscriptions), like sabre's createCollection.
	if len(resourceTypes) > 1 {
		ext, isExt := parent.(ExtendedCollection)
		if !isExt {
			return ErrForbidden("Unable to create a collection of this type")
		}
		if err := ext.CreateExtendedCollection(name, resourceTypes, extraProps); err != nil {
			return err
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusCreated)
		return nil
	}
	if err := coll.CreateDirectory(name); err != nil {
		return err
	}

	// Extended MKCOL properties apply to the fresh collection; on failure
	// the collection is rolled back and the failed statuses reported,
	// mirroring sabre's createCollection.
	if len(extraProps) > 0 {
		names := sortedNames(extraProps)
		result, err := c.server.UpdateProperties(r.Context(), path, &davxml.PropertyUpdate{
			Names:  names,
			Values: extraProps,
		})
		if err != nil {
			return err
		}
		failed := false
		for _, status := range result {
			if status > 299 {
				failed = true
			}
		}
		if failed {
			_ = c.server.Tree.Delete(path)
			resp := propstatResponse(c.server.Href(path), names, result)
			return writeMultiStatus(w, &davxml.MultiStatus{Responses: []davxml.Response{resp}})
		}
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
	return nil
}

// sortedNames returns the keys of m sorted by namespace, then local name, so
// responses listing them are deterministic.
func sortedNames(m map[xml.Name]any) []xml.Name {
	out := make([]xml.Name, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b xml.Name) int {
		if c := strings.Compare(a.Space, b.Space); c != 0 {
			return c
		}
		return strings.Compare(a.Local, b.Local)
	})
	return out
}

// writeMultiStatus writes ms as a 207 response.
func writeMultiStatus(w http.ResponseWriter, ms *davxml.MultiStatus) error {
	out, err := ms.Marshal()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, err = w.Write(out)
	return err
}

func (c *CorePlugin) copy(w http.ResponseWriter, r *http.Request) error {
	path, err := c.server.Path(r)
	if err != nil {
		return err
	}
	info, err := c.copyMoveInfo(r, path)
	if err != nil {
		return err
	}
	if info.destinationExists {
		if err := c.deleteDestination(r, info.destination); err != nil {
			return err
		}
	}
	if err := c.server.Tree.Copy(path, info.destination, info.depth); err != nil {
		return err
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(statusForOverwrite(info.destinationExists))
	return nil
}

func (c *CorePlugin) move(w http.ResponseWriter, r *http.Request) error {
	path, err := c.server.Path(r)
	if err != nil {
		return err
	}
	info, err := c.copyMoveInfo(r, path)
	if err != nil {
		return err
	}
	if info.depth != DepthInfinity {
		return ErrBadRequest(`The HTTP Depth header must only contain "infinity" for MOVE`)
	}
	if info.destinationExists {
		if err := c.deleteDestination(r, info.destination); err != nil {
			return err
		}
	}
	if err := c.server.Tree.Move(path, info.destination); err != nil {
		return err
	}
	// afterMove before afterUnbind, so listeners can transfer state
	// (sabre's ordering note for property storage).
	c.server.fireAfterMove(path, info.destination)
	c.server.fireAfterUnbind(path)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(statusForOverwrite(info.destinationExists))
	return nil
}

// deleteDestination removes the node a COPY or MOVE overwrites, running the
// same unbind hooks as a DELETE of it would.
func (c *CorePlugin) deleteDestination(r *http.Request, destination string) error {
	for _, p := range c.server.Plugins() {
		if hook, ok := p.(BeforeUnbindHook); ok {
			if err := hook.BeforeUnbind(r, destination); err != nil {
				return err
			}
		}
	}
	if err := c.server.Tree.Delete(destination); err != nil {
		return err
	}
	c.server.fireAfterUnbind(destination)
	return nil
}

func statusForOverwrite(overwrote bool) int {
	if overwrote {
		return http.StatusNoContent
	}
	return http.StatusCreated
}

type copyMoveInfo struct {
	destination       string
	destinationExists bool
	depth             int
}

// copyMoveInfo ports sabre's Server::getCopyAndMoveInfo validation chain.
func (c *CorePlugin) copyMoveInfo(r *http.Request, sourcePath string) (*copyMoveInfo, error) {
	// Resolve the source first, so a missing source is a 404 before
	// anything at the destination is touched.
	if _, err := c.server.Tree.NodeForPath(sourcePath); err != nil {
		return nil, err
	}
	destHeader := r.Header.Get("Destination")
	if destHeader == "" {
		return nil, ErrBadRequest("The destination header was not supplied")
	}
	destination, err := c.server.stripBase(destHeader)
	if err != nil {
		return nil, err
	}

	depth := DepthInfinity
	if d := r.Header.Get("Depth"); d != "" {
		if strings.EqualFold(d, "infinity") {
			depth = DepthInfinity
		} else if n, convErr := strconv.Atoi(d); convErr == nil && n >= 0 {
			depth = n
		} else {
			return nil, ErrBadRequest(`The HTTP Depth header may only be "infinity", 0 or a positive integer`)
		}
	}

	overwrite := true
	switch strings.ToUpper(r.Header.Get("Overwrite")) {
	case "", "T":
	case "F":
		overwrite = false
	default:
		return nil, ErrBadRequest("The HTTP Overwrite header should be either T or F")
	}

	destDir, _ := splitPath(destination)
	destParent, err := c.server.Tree.NodeForPath(destDir)
	if err != nil {
		if AsError(err).Status == http.StatusNotFound {
			return nil, ErrConflict("The destination node is not found")
		}
		return nil, err
	}
	if !isCollectionNode(destParent) {
		return nil, ErrUnsupportedMediaType("The destination node is not a collection")
	}

	destinationExists := c.server.Tree.NodeExists(destination)
	if destinationExists && !overwrite {
		return nil, ErrPreconditionFailed("The destination node already exists, and the overwrite header is set to false")
	}

	if destination == sourcePath {
		return nil, ErrForbidden("Source and destination uri are identical.")
	}
	if strings.HasPrefix(destination, sourcePath+"/") {
		return nil, ErrConflict("The destination may not be part of the same subtree as the source path.")
	}
	return &copyMoveInfo{destination: destination, destinationExists: destinationExists, depth: depth}, nil
}

// preferMinimal reports whether the client asked for minimal responses via
// Prefer: return=minimal (or the legacy return-minimal / Brief: t forms).
func preferMinimal(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Brief"), "t") {
		return true
	}
	for _, part := range strings.Split(r.Header.Get("Prefer"), ",") {
		part = strings.TrimSpace(part)
		if strings.EqualFold(part, "return=minimal") || strings.EqualFold(part, "return-minimal") {
			return true
		}
	}
	return false
}

// propNamesOnly collapses discovered properties to a single 200 propstat of
// bare names with empty values — the propname flavor of PROPFIND.
func propNamesOnly(propstats []davxml.Propstat) []davxml.Propstat {
	var names []davxml.Property
	for _, ps := range propstats {
		if ps.Status != http.StatusOK {
			continue
		}
		for _, p := range ps.Props {
			names = append(names, davxml.Property{Name: p.Name})
		}
	}
	if len(names) == 0 {
		return nil
	}
	return []davxml.Propstat{{Status: http.StatusOK, Props: names}}
}

// stripStatus removes propstat groups with the given status.
func stripStatus(propstats []davxml.Propstat, status int) []davxml.Propstat {
	out := propstats[:0]
	for _, ps := range propstats {
		if ps.Status != status {
			out = append(out, ps)
		}
	}
	return out
}

// parseDepth interprets the Depth header, defaulting like sabre's
// getHTTPDepth.
func parseDepth(header string, def int) int {
	if header == "" {
		return def
	}
	if strings.EqualFold(header, "infinity") {
		return DepthInfinity
	}
	n, err := strconv.Atoi(header)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// corePropFind supplies the standard {DAV:} properties, mirroring sabre
// CorePlugin::propFind.
func corePropFind(s *Server, pf *PropFind, node Node) {
	dav := func(local string) xml.Name { return xml.Name{Space: davxml.NSDav, Local: local} }

	pf.Handle(dav("getlastmodified"), func() any {
		if lm := node.LastModified(); !lm.IsZero() {
			return lm
		}
		return nil
	})
	if file, isFile := node.(File); isFile {
		pf.Handle(dav("getcontentlength"), func() any {
			// A negative size means unknown (streaming nodes); sabre omits
			// the property, leaving it a 404 propstat, like GET.
			if size := file.Size(); size >= 0 {
				return size
			}
			return nil
		})
		pf.Handle(dav("getetag"), func() any {
			if etag := file.ETag(); etag != "" {
				return etag
			}
			return nil
		})
		pf.Handle(dav("getcontenttype"), func() any {
			if ct := file.ContentType(); ct != "" {
				return ct
			}
			return nil
		})
	}
	pf.Handle(dav("resourcetype"), func() any {
		return davxml.NameSet(s.ResourceTypesForNode(node))
	})
	pf.Handle(dav("supported-report-set"), func() any {
		reports := davxml.ReportSet{}
		for _, plugin := range s.Plugins() {
			if provider, ok := plugin.(ReportSetProvider); ok {
				reports = append(reports, provider.SupportedReports(node, pf.Path())...)
			}
		}
		return reports
	})
	pf.Handle(dav("supported-method-set"), func() any {
		// Rendered as sabre emits it: <d:supported-method name="..."/>
		// children (the d: prefix is always declared on the multistatus
		// root).
		var b strings.Builder
		for _, m := range s.AllowedMethods(pf.Path()) {
			fmt.Fprintf(&b, `<d:supported-method name="%s"/>`, m)
		}
		return davxml.Raw(b.String())
	})
}

// unescapePath decodes percent-encoding in a URL path, matching sabre's
// decodePath (which decodes each segment but keeps "/" literal).
func unescapePath(p string) (string, error) {
	if !strings.Contains(p, "%") {
		return p, nil
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			return "", err
		}
		// gopath.Clean ran before decoding, so a segment that decodes to
		// ".", ".." or "", or one holding an encoded "/", would smuggle a
		// traversal past normalization. Reject it rather than let it reach
		// a backend as a literal name.
		if dec == "." || dec == ".." || dec == "" || strings.Contains(dec, "/") {
			return "", ErrForbidden("Path contains invalid encoded segment %q", seg)
		}
		segs[i] = dec
	}
	return strings.Join(segs, "/"), nil
}
