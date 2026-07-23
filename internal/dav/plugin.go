package dav

import "net/http"

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
