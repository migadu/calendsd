package dav

import (
	"encoding/xml"
	"net/http"
)

type xmlname = xml.Name

// Plugin extends the server, mirroring sabre's ServerPlugin. The core WebDAV
// behavior is itself a plugin (CorePlugin), so CalDAV, ACL, Sync and Auth all
// hang off the same mechanism.
type Plugin interface {
	// PluginName returns a unique name for the plugin.
	PluginName() string
	// Init is called when the plugin is registered on a server.
	Init(s *Server)
}

// FeatureProvider contributes tokens to the DAV response header
// (e.g. "1", "3", "extended-mkcol", "calendar-access").
type FeatureProvider interface {
	// Features returns DAV header compliance tokens.
	Features() []string
}

// MethodProvider contributes HTTP methods to the Allow header and receives a
// chance to handle them. Replaces sabre's beforeMethod/method:* events.
type MethodProvider interface {
	// Methods returns the HTTP methods this plugin can handle for the node
	// at path. Called for Allow headers; path may not exist.
	Methods(path string) []string
	// HandleMethod handles the request if it can. It returns true if the
	// request was handled and the response written.
	HandleMethod(w http.ResponseWriter, r *http.Request) (handled bool, err error)
}

// PropFindHook lets a plugin contribute property values during PROPFIND.
// Replaces sabre's propFind event.
type PropFindHook interface {
	// PropFind may resolve properties on the PropFind object.
	PropFind(pf *PropFind, node Node) error
}

// BeforeMethodHook runs before method dispatch (and before precondition
// checks), like sabre's beforeMethod event. Auth lives here.
type BeforeMethodHook interface {
	// BeforeMethod returns proceed=false to stop processing (having
	// written the response itself), or an error to abort.
	BeforeMethod(w http.ResponseWriter, r *http.Request) (proceed bool, err error)
}

// PropPatchHook lets a plugin claim PROPPATCH mutations after the node's own
// handlers ran (sabre's propPatch event at priority 300, where property
// storage lives).
type PropPatchHook interface {
	PropPatch(path string, pp *PropPatch) error
}

// LatePropFindHook runs after node-supplied dead properties (sabre's
// priority-130 propFind listeners, i.e. property storage).
type LatePropFindHook interface {
	PropFindLate(pf *PropFind, node Node) error
}

// AfterUnbindHook fires after a node was removed (sabre's afterUnbind).
type AfterUnbindHook interface {
	AfterUnbind(path string) error
}

// AfterMoveHook fires after a MOVE completed (sabre's afterMove; runs before
// AfterUnbind so listeners can transfer state).
type AfterMoveHook interface {
	AfterMove(source, destination string) error
}

// ReportSetProvider advertises the REPORTs a plugin supports on a node, for
// {DAV:}supported-report-set. Replaces sabre's getSupportedReportSet.
type ReportSetProvider interface {
	SupportedReports(node Node, path string) []xml.Name
}

// ReportHandler lets a plugin implement REPORT methods. Replaces sabre's
// report event.
type ReportHandler interface {
	// Report handles the named report if it can; body is the full request
	// body. handled=false passes the report to the next plugin.
	Report(name xmlname, body []byte, path string, w http.ResponseWriter, r *http.Request) (handled bool, err error)
}
