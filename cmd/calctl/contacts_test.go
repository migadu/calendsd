package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/migadu/calendsd/internal/storage"
)

// A report that cannot be written must surface from close, not vanish: the
// run used to drop every Write and Close error on the floor.
func TestImportReportWriteErrorSurfaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.ndjson")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) // read-only: every Write fails
	if err != nil {
		t.Fatal(err)
	}
	r := &importRun{
		opts:   importOptions{reportPath: path},
		counts: map[storage.ImportAction]int{},
		report: f,
	}
	r.record(storage.ImportResult{Principal: "alice@example.org"}, false)
	if err := r.close(); err == nil {
		t.Fatal("close = nil, want the report write error")
	}

	ok, err := os.Create(filepath.Join(t.TempDir(), "ok.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	r = &importRun{counts: map[storage.ImportAction]int{}, report: ok}
	r.record(storage.ImportResult{Principal: "bob@example.org"}, false)
	if err := r.close(); err != nil {
		t.Fatalf("close = %v, want nil", err)
	}
}
