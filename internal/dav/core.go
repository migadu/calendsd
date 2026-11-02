package dav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	davxml "github.com/migadu/calendsd/internal/dav/xml"
	"github.com/migadu/calendsd/internal/pathutil"
)

// CorePlugin implements the baseline WebDAV protocol behavior.
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

// Methods implements MethodProvider. MKCOL is only advertised on unmapped
// URIs.
func (c *CorePlugin) Methods(ctx context.Context, path string) []string {
	methods := []string{
		"OPTIONS", "GET", "HEAD", "DELETE", "PROPFIND",
		"PUT", "PROPPATCH", "COPY", "MOVE", "REPORT",
	}
	if c.server.Tree == nil {
		return append(methods, "MKCOL")
	}
	// The Allow header is advisory: a failed lookup still offers MKCOL.
	if exists, err := c.server.Tree.NodeExists(ctx, path); err != nil || !exists {
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

// report parses the report's root element name and dispatches it to the
// ReportHandler plugins.
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
	for rh := range Hooks[ReportHandler](c.server) {
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
	// MKCOL is only advertised on genuinely unmapped URIs.
	path, err := c.server.Path(r)
	if err != nil {
		path = ""
	}
	h := w.Header()
	h.Set("DAV", strings.Join(c.server.Features(), ", "))
	h.Set("Allow", strings.Join(c.server.AllowedMethods(r.Context(), path), ", "))
	h.Set("MS-Author-Via", "DAV")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
	return nil
}

// get serves file bodies with Range support. With headOnly the body is
// omitted. handled=false means the node is not a file, so another plugin may
// serve it.
func (c *CorePlugin) get(w http.ResponseWriter, r *http.Request, headOnly bool) (handled bool, err error) {
	path, err := c.server.Path(r)
	if err != nil {
		return true, err
	}
	node, err := c.server.Tree.NodeForPath(r.Context(), path)
	if err != nil {
		return true, err
	}

	// Plugins may take over serving (content-type transforms and
	// non-file nodes like notifications). Consulted before the File
	// check so a plugin can serve a node that is not a File.
	if !headOnly {
		for hook := range Hooks[GetHandlerHook](c.server) {
			handled, err := hook.HandleGet(node, w, r)
			if handled || err != nil {
				return true, err
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
	// and no range support.
	size := file.Size()
	if headOnly {
		if size >= 0 {
			h.Set("Content-Length", strconv.FormatInt(size, 10))
		}
		w.WriteHeader(http.StatusOK)
		return true, nil
	}

	body, err := file.Get(r.Context())
	if err != nil {
		return true, err
	}
	defer body.Close()

	if size < 0 {
		w.WriteHeader(http.StatusOK)
		_, err = io.Copy(w, body)
		return true, err
	}
	if rs, ok := body.(io.ReadSeeker); ok {
		// Range, If-Range (against the validators set above) and
		// Content-Length; the other preconditions already passed.
		http.ServeContent(w, r, "", file.LastModified(), rs)
		return true, nil
	}
	// A body that cannot seek is served whole: Range is optional for a
	// server (RFC 7233 §3.1).
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, err = io.Copy(w, body)
	return true, err
}

// head runs GET without a body; 501 (no handler) converts to an empty 200
// for client compatibility.
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
	for hook := range Hooks[BeforeUnbindHook](c.server) {
		if err := hook.BeforeUnbind(r, path); err != nil {
			return err
		}
	}
	if err := c.server.Tree.Delete(r.Context(), path); err != nil {
		return err
	}
	c.server.fireAfterUnbind(r.Context(), path)
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
	logListing := listingLogger(r, path, depth)
	responses, err := c.server.PropertiesForPath(r.Context(), path, names, depth, pf.Include...)
	if err != nil {
		return err
	}
	logListing(len(responses) - 1)
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
	h.Set("Vary", "Brief,Prefer")
	// iCal depends on DAV headers on PROPFIND responses.
	h.Set("DAV", strings.Join(c.server.Features(), ", "))
	return c.server.WriteMultiStatus(w, &davxml.MultiStatus{Responses: responses})
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
	w.Header().Set("Vary", "Brief,Prefer")
	return c.server.WriteMultiStatus(w, &davxml.MultiStatus{Responses: []davxml.Response{resp}})
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
	// Finder's chunked PUTs can arrive empty; fail them to protect the
	// user (X-Expected-Entity-Length announces the intended size).
	if expected := r.Header.Get("X-Expected-Entity-Length"); expected != "" && expected != "0" {
		first := make([]byte, 1)
		n, _ := io.ReadFull(r.Body, first)
		if n != 1 {
			return ErrForbidden("This server is not compatible with OS/X finder. Consider using a different WebDAV client or webserver.")
		}
		body = io.MultiReader(strings.NewReader(string(first[:n])), r.Body)
	}

	exists, err := c.server.Tree.NodeExists(r.Context(), path)
	if err != nil {
		return err
	}
	isNew := !exists

	// Write interceptors (the scheduling broker) may rewrite the payload.
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	sent := raw
	for hook := range Hooks[WriteInterceptor](c.server) {
		raw, err = hook.InterceptWrite(r, path, raw, isNew)
		if err != nil {
			return err
		}
	}
	body = bytes.NewReader(raw)
	// RFC 4791 §5.3.4: a server that stores something other than the body it
	// was sent must not return a strong ETag for it — the client would take
	// the tag to describe its own bytes.
	modified := !bytes.Equal(sent, raw)

	if !isNew {
		node, err := c.server.Tree.NodeForPath(r.Context(), path)
		if err != nil {
			return err
		}
		file, isFile := node.(File)
		if !isFile {
			return ErrConflict("PUT is not allowed on non-files.")
		}
		var etag string
		ifMatch := r.Header.Get("If-Match")
		if cp, ok := node.(ConditionalPutter); ok && ifMatch != "" && ifMatch != "*" {
			// checkPreconditions already matched If-Match against the node,
			// but a writer can land between that check and this write. Make
			// the write itself conditional on the tag that matched.
			current := file.ETag()
			if !anyETagMatches(ifMatch, current) {
				return ErrPreconditionFailed("An If-Match header was specified, but none of the specified ETags matched.")
			}
			etag, err = cp.PutIfMatch(r.Context(), raw, current)
			if errors.Is(err, ErrETagMismatch) {
				return ErrPreconditionFailed("The resource was modified by another request while this PUT was in progress; refresh and retry.")
			}
		} else {
			etag, err = file.Put(r.Context(), body)
		}
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

	parentPath, name := pathutil.Split(path)
	if parentExists, err := c.server.Tree.NodeExists(r.Context(), parentPath); err != nil {
		return err
	} else if !parentExists {
		return ErrConflict("Files can not be created in non-existent collections")
	}
	parent, err := c.server.Tree.NodeForPath(r.Context(), parentPath)
	if err != nil {
		return err
	}
	coll, isColl := parent.(Collection)
	if !isColl {
		return ErrConflict("Files can only be created as children of collections")
	}
	etag, err := coll.CreateFile(r.Context(), name, body)
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

// fireAfterWrite notifies AfterWriteHook plugins that a PUT has been
// persisted.
func (c *CorePlugin) fireAfterWrite(r *http.Request, path string) {
	for hook := range Hooks[AfterWriteHook](c.server) {
		hook.AfterWrite(r, path)
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

	if exists, err := c.server.Tree.NodeExists(r.Context(), path); err != nil {
		return err
	} else if exists {
		return ErrMethodNotAllowed("The resource you tried to create already exists")
	}
	parentPath, name := pathutil.Split(path)
	parent, err := c.server.Tree.NodeForPath(r.Context(), parentPath)
	if err != nil {
		if IsNotFound(err) {
			return ErrConflict("Parent node does not exist")
		}
		return err
	}
	coll, isColl := parent.(Collection)
	if !isColl {
		return ErrConflict("Parent node is not a collection")
	}
	// Non-plain resource types go to extended-collection parents
	// (calendars, subscriptions).
	if len(resourceTypes) > 1 {
		ext, isExt := parent.(ExtendedCollection)
		if !isExt {
			return ErrForbidden("Unable to create a collection of this type")
		}
		if err := ext.CreateExtendedCollection(r.Context(), name, resourceTypes, extraProps); err != nil {
			return err
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusCreated)
		return nil
	}
	if err := coll.CreateDirectory(r.Context(), name); err != nil {
		return err
	}

	// Extended MKCOL properties apply to the fresh collection; on failure
	// the collection is rolled back and the failed statuses reported.
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
			_ = c.server.Tree.Delete(r.Context(), path)
			resp := propstatResponse(c.server.Href(path), names, result)
			return c.server.WriteMultiStatus(w, &davxml.MultiStatus{Responses: []davxml.Response{resp}})
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
	if err := c.server.Tree.Copy(r.Context(), path, info.destination, info.depth); err != nil {
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
	if err := c.server.Tree.Move(r.Context(), path, info.destination); err != nil {
		return err
	}
	// AfterMove before AfterUnbind, so listeners such as property storage
	// can transfer state.
	c.server.fireAfterMove(r.Context(), path, info.destination)
	c.server.fireAfterUnbind(r.Context(), path)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(statusForOverwrite(info.destinationExists))
	return nil
}

// deleteDestination removes the node a COPY or MOVE overwrites, running the
// same unbind hooks as a DELETE of it would.
func (c *CorePlugin) deleteDestination(r *http.Request, destination string) error {
	for hook := range Hooks[BeforeUnbindHook](c.server) {
		if err := hook.BeforeUnbind(r, destination); err != nil {
			return err
		}
	}
	if err := c.server.Tree.Delete(r.Context(), destination); err != nil {
		return err
	}
	c.server.fireAfterUnbind(r.Context(), destination)
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

// copyMoveInfo validates the Destination, Depth and Overwrite headers of a
// COPY or MOVE and resolves the destination.
func (c *CorePlugin) copyMoveInfo(r *http.Request, sourcePath string) (*copyMoveInfo, error) {
	// Resolve the source first, so a missing source is a 404 before
	// anything at the destination is touched.
	if _, err := c.server.Tree.NodeForPath(r.Context(), sourcePath); err != nil {
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

	destDir, _ := pathutil.Split(destination)
	destParent, err := c.server.Tree.NodeForPath(r.Context(), destDir)
	if err != nil {
		if IsNotFound(err) {
			return nil, ErrConflict("The destination node is not found")
		}
		return nil, err
	}
	if !isCollectionNode(destParent) {
		return nil, ErrUnsupportedMediaType("The destination node is not a collection")
	}

	destinationExists, err := c.server.Tree.NodeExists(r.Context(), destination)
	if err != nil {
		return nil, err
	}
	if destinationExists && !overwrite {
		return nil, ErrPreconditionFailed("The destination node already exists, and the overwrite header is set to false")
	}

	if destination == sourcePath {
		return nil, ErrForbidden("Source and destination uri are identical.")
	}
	if strings.HasPrefix(destination, sourcePath+"/") {
		return nil, ErrConflict("The destination may not be part of the same subtree as the source path.")
	}
	if destination == "" || strings.HasPrefix(sourcePath, destination+"/") {
		// The converse: a COPY/MOVE of an object onto its own collection with
		// Overwrite would delete the collection (and the source with it)
		// before the copy could fail (audit 6.19).
		return nil, ErrForbidden("The destination may not be an ancestor of the source path.")
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

// parseDepth interprets the Depth header: "infinity" is DepthInfinity, and
// a missing or invalid value yields def.
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

// corePropFind supplies the standard {DAV:} properties.
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
			// A negative size means unknown (streaming nodes); omit the
			// property, leaving it a 404 propstat, like GET.
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
		for provider := range Hooks[ReportSetProvider](s) {
			reports = append(reports, provider.SupportedReports(node, pf.Path())...)
		}
		return reports
	})
	pf.Handle(dav("supported-method-set"), func() any {
		return davxml.SupportedMethodSet(s.AllowedMethods(pf.Context(), pf.Path()))
	})
}

// unescapePath decodes percent-encoding in a URL path, segment by segment,
// keeping "/" literal.
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
		// Names are stored as Postgres text, which refuses invalid UTF-8
		// and NUL; reject them here as the client's error, not a 500.
		if !utf8.ValidString(dec) || strings.ContainsRune(dec, 0) {
			return "", ErrBadRequest("Path segment %q is not valid UTF-8 text", seg)
		}
		segs[i] = dec
	}
	return strings.Join(segs, "/"), nil
}
