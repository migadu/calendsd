package corpus

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"time"

	"calserver/internal/acl"
	"calserver/internal/auth"
	"calserver/internal/dav"
	"calserver/internal/dav/mock"
	davxml "calserver/internal/dav/xml"
)

type xmlName = xml.Name

// BuildServer constructs a Go server equivalent to the sabre test setup of a
// record: seeded tree, principal backend, auth (autoLogin) and ACL plugins.
// Mirrors AbstractDAVServerTestCase::initializeEverything.
func BuildServer(rec *Record) (*dav.Server, error) {
	principals, err := seedPrincipals(rec.Setup.Principals)
	if err != nil {
		return nil, err
	}
	backend := &acl.MemoryBackend{Principals: principals}

	root, err := seedTree(rec.Request.Tree, backend)
	if err != nil {
		return nil, err
	}
	srv := dav.NewServer("/", root)

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
func seedTree(t *Tree, backend acl.Backend) (dav.Collection, error) {
	if t == nil {
		return nil, fmt.Errorf("record has no tree fixture")
	}
	if t.Type == "roots" {
		// sabre wraps a node array in a root SimpleCollection named "root".
		root := mock.NewCollection("root")
		for _, r := range t.Roots {
			node, err := seedNode(r, backend)
			if err != nil {
				return nil, err
			}
			root.Attach(node)
		}
		return root, nil
	}
	node, err := seedNode(t, backend)
	if err != nil {
		return nil, err
	}
	coll, ok := node.(dav.Collection)
	if !ok {
		return nil, fmt.Errorf("root fixture %q is not a collection", t.Name)
	}
	return coll, nil
}

func seedNode(t *Tree, backend acl.Backend) (dav.Node, error) {
	switch t.Type {
	case "file":
		f := mock.NewFile(t.Name, t.Contents)
		if t.LastModified > 0 {
			f.SetLastModified(time.Unix(t.LastModified, 0))
		}
		return f, nil
	case "streamingfile":
		size := int64(-1)
		if t.Size != nil {
			size = *t.Size
		}
		return mock.NewStreamingFile(t.Name, t.Contents, size), nil
	case "collection":
		c := mock.NewCollection(t.Name)
		for _, child := range t.Children {
			node, err := seedNode(child, backend)
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
			node, err := seedNode(child, backend)
			if err != nil {
				return nil, err
			}
			pc.Attach(node)
		}
		return pc, nil
	case "node":
		switch t.Class {
		case `Sabre\DAVACL\PrincipalCollection`, `Sabre\CalDAV\Principal\Collection`:
			return acl.NewPrincipalCollection(backend, t.Name), nil
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
