package dav

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	davxml "calserver/internal/dav/xml"
)

// TempFileFilterPlugin intercepts the garbage files operating systems and
// applications create when a WebDAV share is mounted as a disk (OS X resource
// forks and .DS_Store, Windows desktop.ini/Thumbs.db, editor swap files) and
// keeps them out of the calendar/contact backend. A faithful port of sabre's
// TemporaryFileFilterPlugin: matching names are served from a scratch
// directory on disk instead of the tree, so a client's junk PUT no longer hits
// the calendar-data validator (415) or ACL (403) on a real collection.
//
// The files are never deleted automatically; point DataDir at something an
// external sweep prunes (sabre suggests a 24h expiry). Opt-in, like sabre's
// example server which adds it explicitly — calserver core does not run it by
// default so the pinned corpus (sabre's own unit suite, which has no temp
// filter) is unaffected.
type TempFileFilterPlugin struct {
	server *Server
	// DataDir is where intercepted files are stored. Required; created lazily
	// on first write.
	DataDir string
	// Patterns is the set of basename patterns treated as temporary. Nil uses
	// the sabre default set.
	Patterns []*regexp.Regexp

	mu    sync.Mutex
	usage *tempUsage // nil until first counted; see reserve
}

// Limits on what the scratch directory may hold, so the filter cannot be used
// to fill the disk: per file, in total, and by count.
const (
	maxTempFileSize  = 10 << 20
	maxTempDirBytes  = 256 << 20
	maxTempFileCount = 10000
)

type tempUsage struct {
	bytes int64
	files int
}

// privilegeChecker is the ACL plugin's enforcement surface (acl.Plugin), named
// here structurally since dav cannot import acl.
type privilegeChecker interface {
	CheckPrivileges(ctx context.Context, path string, privileges ...string) error
}

// defaultTempFilePatterns mirrors sabre's temporaryFilePatterns. The literal
// dots that sabre left unescaped (matching any char) are escaped here — the
// intent is the fixed filenames, and the practical match set is identical.
var defaultTempFilePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^\._`),           // OS X resource forks
	regexp.MustCompile(`^\.DS_Store$`),   // OS X custom folder settings
	regexp.MustCompile(`^desktop\.ini$`), // Windows custom folder settings
	regexp.MustCompile(`^Thumbs\.db$`),   // Windows thumbnail cache
	regexp.MustCompile(`^\..*\.swp$`),    // ViM swap files
	regexp.MustCompile(`^\.dat`),         // Smultron temp files
	regexp.MustCompile(`^~lock\..*#$`),   // Windows 7 lock files
}

func (p *TempFileFilterPlugin) PluginName() string { return "temp-file-filter" }

func (p *TempFileFilterPlugin) Init(s *Server) {
	p.server = s
	if p.Patterns == nil {
		p.Patterns = defaultTempFilePatterns
	}
}

// BeforeMethod runs after auth (so temp requests are still authenticated) and
// before precondition checks and the core method handlers, matching sabre's
// beforeMethod:*. A non-temp path, or a GET/DELETE/PROPFIND on a temp name that
// was never written, returns proceed=true to fall through to normal handling
// (which 404s the nonexistent node, exactly as sabre does).
func (p *TempFileFilterPlugin) BeforeMethod(w http.ResponseWriter, r *http.Request) (bool, error) {
	reqPath, err := p.server.Path(r)
	if err != nil {
		// A malformed path is not ours to answer; let normal handling report it.
		return true, nil
	}
	loc, ok := p.tempLocation(Principal(r.Context()), reqPath)
	if !ok {
		return true, nil
	}
	// The filter answers in place of the tree, so it must not answer anyone
	// the tree would refuse: require the right to create a file in the parent
	// collection. Otherwise fall through, and the ACL plugin and the core
	// handlers answer as for any other path.
	if !p.mayBind(r, reqPath) {
		return true, nil
	}
	switch r.Method {
	case "GET":
		return p.get(w, loc)
	case "PUT":
		return p.put(w, r, loc)
	case "DELETE":
		return p.delete(w, loc)
	case "PROPFIND":
		return p.propfind(w, reqPath, loc)
	}
	return true, nil
}

// mayBind reports whether the requester holds {DAV:}bind on reqPath's parent.
// A server without access control allows everything, so the filter does too.
func (p *TempFileFilterPlugin) mayBind(r *http.Request, reqPath string) bool {
	parent := path.Dir("/" + reqPath)[1:]
	for _, plugin := range p.server.Plugins() {
		if checker, ok := plugin.(privilegeChecker); ok {
			return checker.CheckPrivileges(r.Context(), parent, "{DAV:}bind") == nil
		}
	}
	return true
}

// tempLocation returns the on-disk path for a request path whose basename
// matches a temporary-file pattern, keyed by md5 of the principal and the full
// path, so the same junk name in different collections or from different users
// does not collide.
func (p *TempFileFilterPlugin) tempLocation(principal, reqPath string) (string, bool) {
	base := path.Base(reqPath)
	if base == "." || base == "/" || base == "" {
		return "", false
	}
	matched := false
	for _, re := range p.Patterns {
		if re.MatchString(base) {
			matched = true
			break
		}
	}
	if !matched {
		return "", false
	}
	sum := md5.Sum([]byte(principal + "\x00" + reqPath))
	return filepath.Join(p.DataDir, "calserver_"+hex.EncodeToString(sum[:])+".tempfile"), true
}

func (p *TempFileFilterPlugin) get(w http.ResponseWriter, loc string) (bool, error) {
	f, err := os.Open(loc)
	if os.IsNotExist(err) {
		return true, nil // never written: fall through to a normal 404
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	h.Set("X-Sabre-Temp", "true")
	w.WriteHeader(http.StatusOK)
	_, err = io.Copy(w, f)
	return false, err
}

func (p *TempFileFilterPlugin) put(w http.ResponseWriter, r *http.Request, loc string) (bool, error) {
	if err := os.MkdirAll(p.DataDir, 0o700); err != nil {
		return false, err
	}
	_, statErr := os.Stat(loc)
	newFile := os.IsNotExist(statErr)
	if !newFile {
		if statErr != nil {
			return false, statErr
		}
		if r.Header.Get("If-None-Match") != "" {
			return false, ErrPreconditionFailed(
				"The resource already exists, and an If-None-Match header was supplied")
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTempFileSize+1))
	if err != nil {
		return false, err
	}
	if len(body) > maxTempFileSize {
		return false, &Error{Status: http.StatusRequestEntityTooLarge,
			Message: "Temporary files are limited to " + strconv.Itoa(maxTempFileSize) + " bytes"}
	}
	var oldSize int64
	if !newFile {
		if info, err := os.Stat(loc); err == nil {
			oldSize = info.Size()
		}
	}
	if err := p.reserve(int64(len(body))-oldSize, boolToInt(newFile)); err != nil {
		return false, err
	}
	if err := os.WriteFile(loc, body, 0o600); err != nil {
		p.release(int64(len(body))-oldSize, boolToInt(newFile))
		return false, err
	}
	w.Header().Set("X-Sabre-Temp", "true")
	if newFile {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	return false, nil
}

func (p *TempFileFilterPlugin) delete(w http.ResponseWriter, loc string) (bool, error) {
	if _, err := os.Stat(loc); os.IsNotExist(err) {
		return true, nil // never written: fall through to a normal 404
	}
	info, err := os.Stat(loc)
	if err != nil {
		return false, err
	}
	if err := os.Remove(loc); err != nil {
		return false, err
	}
	p.release(info.Size(), 1)
	w.Header().Set("X-Sabre-Temp", "true")
	w.WriteHeader(http.StatusNoContent)
	return false, nil
}

// propfind answers with a fixed minimal property set (sabre's "lazy" propfind:
// it does not parse the requested properties), enough for a client that PUT a
// temp file to then stat it.
func (p *TempFileFilterPlugin) propfind(w http.ResponseWriter, reqPath, loc string) (bool, error) {
	info, err := os.Stat(loc)
	if os.IsNotExist(err) {
		return true, nil // never written: fall through to a normal 404
	}
	if err != nil {
		return false, err
	}
	dav := func(local string) xml.Name { return xml.Name{Space: davxml.NSDav, Local: local} }
	resp := davxml.Response{
		Href: p.server.BaseURI + encodePath(reqPath),
		Propstats: []davxml.Propstat{{
			Status: http.StatusOK,
			Props: []davxml.Property{
				{Name: dav("getlastmodified"), Value: toXMLValue(info.ModTime())},
				{Name: dav("getcontentlength"), Value: toXMLValue(info.Size())},
				{Name: dav("resourcetype"), Value: davxml.NameSet(nil)},
				{Name: xml.Name{Space: davxml.NSSabreDAV, Local: "tempFile"}, Value: davxml.Text("true")},
			},
		}},
	}
	body, err := (&davxml.MultiStatus{Responses: []davxml.Response{resp}}).Marshal()
	if err != nil {
		return false, err
	}
	h := w.Header()
	h.Set("Content-Type", "application/xml; charset=utf-8")
	h.Set("X-Sabre-Temp", "true")
	w.WriteHeader(http.StatusMultiStatus)
	_, err = w.Write(body)
	return false, err
}

// reserve accounts for a write of delta bytes and newFiles files, refusing it
// with 507 when the directory would exceed its limits. Usage is counted once
// from disk and tracked in memory after that; files an external sweep removes
// are noticed by recounting before a write is refused.
func (p *TempFileFilterPlugin) reserve(delta int64, newFiles int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	fits := func() bool {
		return p.usage.bytes+delta <= maxTempDirBytes && p.usage.files+newFiles <= maxTempFileCount
	}
	if p.usage == nil || !fits() {
		usage, err := p.countUsage()
		if err != nil {
			return err
		}
		p.usage = usage
	}
	if !fits() {
		return &Error{Status: http.StatusInsufficientStorage, Message: "The temporary file area is full"}
	}
	p.usage.bytes += delta
	p.usage.files += newFiles
	return nil
}

func (p *TempFileFilterPlugin) release(bytes int64, files int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.usage != nil {
		p.usage.bytes -= bytes
		p.usage.files -= files
	}
}

func (p *TempFileFilterPlugin) countUsage() (*tempUsage, error) {
	entries, err := os.ReadDir(p.DataDir)
	if err != nil {
		return nil, err
	}
	usage := &tempUsage{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "calserver_") || !strings.HasSuffix(e.Name(), ".tempfile") {
			continue
		}
		if info, err := e.Info(); err == nil {
			usage.bytes += info.Size()
			usage.files++
		}
	}
	return usage, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
