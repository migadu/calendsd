package storage_test

import (
	"encoding/xml"
	"testing"

	"github.com/migadu/calendsd/internal/dav"
	davxml "github.com/migadu/calendsd/internal/dav/xml"
	"github.com/migadu/calendsd/internal/storage"
	"github.com/migadu/calendsd/internal/storage/storagetest"
)

// PropFindMany reads what PropFind reads for each path, in one query, and
// returns no entry for a path without properties or with only rows this
// server does not serve.
func TestPropFindManyMatchesPropFind(t *testing.T) {
	db, qlog := storagetest.NewLoggedDB(t, "calserver_b_propstorage_many")
	mustExec(t, db, `
		INSERT INTO propertystorage (path, name, valuetype, value) VALUES
			('calendars/a/x', '{DAV:}details', 1, 'x1'),
			('calendars/a/x', '{urn:x}raw', 2, '<x>1</x>'),
			('calendars/a/y', '{DAV:}details', 1, 'y1'),
			('calendars/a/php', '{DAV:}details', 3, 'O:1'),
			('calendars/a/badname', 'not-clark', 1, 'v'),
			('calendars/b/x', '{DAV:}details', 1, 'other');`)
	store := &storage.PropertyStorage{DB: db}
	paths := []string{"calendars/a/x", "calendars/a/y", "calendars/a/z", "calendars/a/php", "calendars/a/badname"}

	qlog.Reset()
	many, err := store.PropFindMany(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if n := qlog.Count(); n != 1 {
		t.Errorf("PropFindMany ran %d queries, want 1", n)
	}
	if len(many) != 2 || len(many["calendars/a/x"]) != 2 || len(many["calendars/a/y"]) != 1 {
		t.Errorf("PropFindMany = %v", many)
	}
	for _, path := range paths {
		pf := dav.NewPropFind(t.Context(), path, []xml.Name{{Space: "DAV:", Local: "details"}, {Space: "urn:x", Local: "raw"}}, 0)
		if err := store.PropFind(t.Context(), path, pf); err != nil {
			t.Fatal(err)
		}
		want := map[xml.Name]any{}
		for _, name := range pf.Requested() {
			if v := pf.Get(name); v != nil {
				want[name] = v
			}
		}
		got := map[xml.Name]any{}
		for _, prop := range many[path] {
			got[prop.Name] = prop.Value
		}
		if len(got) != len(want) {
			t.Errorf("%s: PropFindMany %v, PropFind %v", path, got, want)
			continue
		}
		for name, v := range want {
			if gv, ok := got[name]; !ok || toString(gv) != toString(v) {
				t.Errorf("%s %v: PropFindMany %v, PropFind %v", path, name, gv, v)
			}
		}
	}
}

func toString(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case davxml.Raw:
		return string(v)
	}
	return ""
}
