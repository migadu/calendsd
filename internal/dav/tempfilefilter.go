package dav

import (
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
	loc, ok := p.tempLocation(reqPath)
	if !ok {
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

// tempLocation returns the on-disk path for a request path whose basename
// matches a temporary-file pattern, keyed by md5 of the full path so the same
// junk name in different collections does not collide (sabre's scheme).
func (p *TempFileFilterPlugin) tempLocation(reqPath string) (string, bool) {
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
	sum := md5.Sum([]byte(reqPath))
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
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(loc, body, 0o600); err != nil {
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
	if err := os.Remove(loc); err != nil {
		return false, err
	}
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
