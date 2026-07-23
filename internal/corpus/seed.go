package corpus

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/caldav"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	davxml "calserver/internal/dav/xml"
)

type xmlName = xml.Name

// BuildServer constructs a Go server equivalent to the sabre test setup of a
// record: seeded tree, principal/calendar backends, auth (autoLogin), ACL and
// CalDAV plugins. Mirrors AbstractDAVServerTestCase::initializeEverything.
func BuildServer(rec *Record) (*dav.Server, error) {
	principals, err := seedPrincipals(rec.Setup.Principals)
	if err != nil {
		return nil, err
	}
	backends := &seedBackends{principals: &acl.MemoryBackend{Principals: principals}}
	if rec.Setup.SetupCalDAV {
		backends.caldav, err = seedCalDAV(rec)
		if err != nil {
			return nil, err
		}
	}

	root, err := seedTree(rec.Request.Tree, backends)
	if err != nil {
		return nil, err
	}
	srv := dav.NewServer("/", root)
	// The sync plugin is part of the standard server (sabre's test harness
	// adds it in the sync suite; it is inert elsewhere).
	srv.AddPlugin(&dav.SyncPlugin{})

	if rec.Setup.SetupCalDAV {
		srv.AddPlugin(&caldav.Plugin{})
	}
	if rec.Setup.AutoLogin != "" {
		srv.AddPlugin(auth.NewLoggedInPlugin("principals/" + rec.Setup.AutoLogin))
	}
	if rec.Setup.SetupACL {
		srv.AddPlugin(&acl.Plugin{
			AdminPrincipals:            []string{"principals/admin"},
			AllowUnauthenticatedAccess: rec.Setup.AutoLogin == "",
		})
	}
	return srv, nil
}

type seedBackends struct {
	principals acl.Backend
	caldav     caldav.Backend
}

// seedCalDAV parses the caldavCalendars/caldavCalendarObjects fixtures into
// a mock calendar backend.
func seedCalDAV(rec *Record) (caldav.Backend, error) {
	backend := &caldav.MockBackend{Objects: map[caldav.CalendarID]map[string]*caldav.ObjectInfo{}}

	if len(rec.Setup.Calendars) > 0 {
		var entries []map[string]any
		if err := json.Unmarshal(rec.Setup.Calendars, &entries); err != nil {
			return nil, fmt.Errorf("parsing caldavCalendars: %w", err)
		}
		for _, entry := range entries {
			info := caldav.CalendarInfo{Props: map[xmlName]any{}}
			for k, v := range entry {
				switch k {
				case "id":
					info.ID = caldav.CalendarID{Calendar: jsonID(v)}
				case "uri":
					info.URI, _ = v.(string)
				case "principaluri":
					info.PrincipalURI, _ = v.(string)
				case "name":
					// sabre's mock keeps a non-property name key.
				default:
					name, err := davxml.ParseClark(k)
					if err != nil {
						continue
					}
					value, err := seedCalendarProp(v)
					if err != nil {
						return nil, fmt.Errorf("calendar prop %s: %w", k, err)
					}
					info.Props[name] = value
				}
			}
			backend.Calendars = append(backend.Calendars, info)
		}
	}

	if len(rec.Setup.CalendarObjects) > 0 {
		var perCalendar map[string]map[string]struct {
			CalendarData string `json:"calendardata"`
		}
		if err := json.Unmarshal(rec.Setup.CalendarObjects, &perCalendar); err != nil {
			return nil, fmt.Errorf("parsing caldavCalendarObjects: %w", err)
		}
		for calID, objects := range perCalendar {
			id := caldav.CalendarID{Calendar: calID}
			backend.Objects[id] = map[string]*caldav.ObjectInfo{}
			for uri, obj := range objects {
				backend.Objects[id][uri] = &caldav.ObjectInfo{
					URI:          uri,
					CalendarID:   id,
					CalendarData: []byte(obj.CalendarData),
				}
			}
		}
	}
	return backend, nil
}

// jsonID renders numeric or string fixture IDs uniformly.
func jsonID(v any) string {
	switch id := v.(type) {
	case string:
		return id
	case float64:
		return fmt.Sprintf("%.0f", id)
	default:
		return fmt.Sprint(id)
	}
}

// seedCalendarProp converts a recorded calendar property value.
func seedCalendarProp(v any) (any, error) {
	switch value := v.(type) {
	case string:
		return value, nil
	case map[string]any:
		if value["__type"] == "supported-calendar-component-set" {
			var b strings.Builder
			if comps, ok := value["components"].([]any); ok {
				for _, c := range comps {
					fmt.Fprintf(&b, `<cal:comp name="%s"/>`, c)
				}
			}
			return davxml.Raw(b.String()), nil
		}
		return nil, fmt.Errorf("unsupported fixture value %v", value["__type"])
	default:
		return nil, fmt.Errorf("unsupported fixture value type %T", v)
	}
}

// seedPrincipals parses the recorded principal fixtures.
func seedPrincipals(raw json.RawMessage) ([]acl.Principal, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parsing principals fixture: %w", err)
	}
	var out []acl.Principal
	for _, entry := range entries {
		p := acl.Principal{Props: map[xml.Name]any{}}
		for k, v := range entry {
			if k == "uri" {
				p.URI, _ = v.(string)
				continue
			}
			name, err := davxml.ParseClark(k)
			if err != nil {
				name = xmlName{Local: k}
			}
			p.Props[name] = v
		}
		out = append(out, p)
	}
	return out, nil
}

// seedTree builds an in-memory node tree from a recorded fixture. It returns
// an error for fixtures using node types the port cannot represent yet; the
// runner reports those records as pending.
func seedTree(t *Tree, backends *seedBackends) (dav.Collection, error) {
	if t == nil {
		return nil, fmt.Errorf("record has no tree fixture")
	}
	if t.Type == "roots" {
		// sabre wraps a node array in a root SimpleCollection named "root".
		root := mock.NewCollection("root")
		for _, r := range t.Roots {
			node, err := seedNode(r, backends)
			if err != nil {
				return nil, err
			}
			root.Attach(node)
		}
		return root, nil
	}
	node, err := seedNode(t, backends)
	if err != nil {
		return nil, err
	}
	coll, ok := node.(dav.Collection)
	if !ok {
		return nil, fmt.Errorf("root fixture %q is not a collection", t.Name)
	}
	return coll, nil
}

func seedNode(t *Tree, backends *seedBackends) (dav.Node, error) {
	switch t.Type {
	case "file":
		f := mock.NewFile(t.Name, t.Contents)
		if t.LastModified > 0 {
			f.SetLastModified(time.Unix(t.LastModified, 0))
		}
		return f, nil
	case "simplefile":
		return mock.NewSimpleFile(t.Name, t.Contents), nil
	case "synccollection":
		sc := mock.NewSyncCollection(t.Name)
		sc.Token = t.Token
		for _, entry := range t.ChangeLog {
			sc.ChangeLog = append(sc.ChangeLog, mock.ChangeEntry{
				Token: entry.Token, Added: entry.Added,
				Modified: entry.Modified, Deleted: entry.Deleted,
			})
		}
		for _, child := range t.Children {
			node, err := seedNode(child, backends)
			if err != nil {
				return nil, err
			}
			sc.Attach(node)
		}
		return sc, nil
	case "streamingfile":
		size := int64(-1)
		if t.Size != nil {
			size = *t.Size
		}
		return mock.NewStreamingFile(t.Name, t.Contents, size), nil
	case "collection":
		c := mock.NewCollection(t.Name)
		for _, child := range t.Children {
			node, err := seedNode(child, backends)
			if err != nil {
				return nil, err
			}
			c.Attach(node)
		}
		return c, nil
	case "propscollection":
		props, err := seedProps(t.Properties)
		if err != nil {
			return nil, err
		}
		pc := mock.NewPropCollection(t.Name, props)
		if mode, isString := t.FailMode.(string); isString {
			pc.Mode = mock.FailMode(mode)
		}
		for _, child := range t.Children {
			node, err := seedNode(child, backends)
			if err != nil {
				return nil, err
			}
			pc.Attach(node)
		}
		return pc, nil
	case "node":
		switch t.Class {
		case `Sabre\DAVACL\PrincipalCollection`, `Sabre\CalDAV\Principal\Collection`:
			return acl.NewPrincipalCollection(backends.principals, t.Name), nil
		case `Sabre\CalDAV\CalendarRoot`:
			if backends.caldav == nil {
				return nil, fmt.Errorf("CalendarRoot fixture without caldav setup")
			}
			return caldav.NewCalendarRoot(backends.principals, backends.caldav, t.Name), nil
		}
		return nil, fmt.Errorf("unseedable node type %q (class %s)", t.Type, t.Class)
	default:
		return nil, fmt.Errorf("unseedable node type %q (class %s)", t.Type, t.Class)
	}
}

func seedProps(raw any) (map[xmlName]any, error) {
	props, isMap := raw.(map[string]any)
	if !isMap {
		// PHP encodes an empty map as []; anything else non-map is
		// unrepresentable.
		if raw == nil {
			return nil, nil
		}
		if _, isList := raw.([]any); isList {
			return nil, nil
		}
		return nil, fmt.Errorf("unsupported properties fixture %T", raw)
	}
	out := make(map[xmlName]any, len(props))
	for clark, v := range props {
		name, err := davxml.ParseClark(clark)
		if err != nil {
			// sabre mock tests may use non-clark names like "my-prop";
			// they live in the empty namespace.
			name = xmlName{Local: clark}
		}
		s, isString := v.(string)
		if !isString {
			return nil, fmt.Errorf("unsupported property value %T for %s", v, clark)
		}
		out[name] = s
	}
	return out, nil
}
