package main

// Bundle-reader tests, including the cross-language contract with the PHP
// exporter on the webmail side (docs/webmail-contacts.md § The bundle).
//
// That contract is the riskiest seam in the migration and the least visible: the
// exporter is PHP on the webmail host, the importer is Go wherever the DSN
// reaches, and the only thing tying them together is a file format. A digest
// function that disagreed by a byte would not fail — it would make every
// principal look changed on every run, which is the "rewrite all 2.96M cards"
// outcome the whole design exists to avoid, arriving for a reason nobody would
// think to look for. So the digest is pinned to a vector produced by the PHP
// side rather than to the Go implementation's own output.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPrincipalDigestMatchesExporter pins the sentinel digest to a value
// computed by the PHP DigestAccumulator:
//
//	$d = new DigestAccumulator();
//	$d->add("a.vcf", "BEGIN:VCARD\r\nEND:VCARD\r\n");
//	$d->add("b.vcf", "x");
//	echo $d->final();
//
// If this test fails, the two sides have drifted — fix whichever moved, do not
// update the constant to match Go.
func TestPrincipalDigestMatchesExporter(t *testing.T) {
	const fromPHP = "dc9ae142c3437bf603ae54324938a50f80da43a9012ea4181a930def9d766a45"
	got := principalDigest([]cardRecord{
		{URI: "a.vcf", Card: "BEGIN:VCARD\r\nEND:VCARD\r\n"},
		{URI: "b.vcf", Card: "x"},
	})
	if got != fromPHP {
		t.Errorf("principalDigest = %s\nthe PHP exporter computes %s", got, fromPHP)
	}
}

// TestScanCardShard walks the framing: records grouped per principal, closed by
// a sentinel whose count and digest must both agree.
func TestScanCardShard(t *testing.T) {
	dir := t.TempDir()
	alice := []cardRecord{
		{Principal: "alice@example.org", URI: "u1.vcf", Card: "BEGIN:VCARD\r\nUID:u1\r\nEND:VCARD\r\n", MTime: 17},
		{Principal: "alice@example.org", URI: "u2.vcf", Card: "BEGIN:VCARD\r\nUID:u2\r\nEND:VCARD\r\n"},
	}
	writeShard(t, dir, "001", append(alice, cardRecord{
		Principal: "alice@example.org", End: true, N: 2,
		Digest: principalDigest(alice), Users: []int32{5},
	}))

	b := openTestBundle(t, dir)
	var got []principalRecords
	if err := b.scanCardShard("001", func(pr principalRecords) error {
		got = append(got, pr)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d principals, want 1", len(got))
	}
	if got[0].Principal != "alice@example.org" || len(got[0].Cards) != 2 {
		t.Errorf("got %s with %d cards", got[0].Principal, len(got[0].Cards))
	}
	if len(got[0].Users) != 1 || got[0].Users[0] != 5 {
		t.Errorf("source users = %v, want [5]", got[0].Users)
	}
}

// TestScanCardShardRejectsTruncation: an export killed mid-principal leaves
// records with no sentinel. Imported as-is it would look clean and simply be
// missing contacts — the one failure mode nothing downstream can detect — so it
// has to be an error here.
func TestScanCardShardRejectsTruncation(t *testing.T) {
	dir := t.TempDir()
	writeShard(t, dir, "002", []cardRecord{
		{Principal: "bob@example.org", URI: "u1.vcf", Card: "BEGIN:VCARD\r\nEND:VCARD\r\n"},
	})
	b := openTestBundle(t, dir)
	err := b.scanCardShard("002", func(principalRecords) error { return nil })
	if err == nil {
		t.Fatal("a shard with no sentinel was accepted")
	}
}

// TestScanCardShardRejectsWrongDigest: the sentinel is what lets the importer
// decide "nothing changed" in one comparison, so a sentinel that does not
// describe its own records makes every later decision unsound.
func TestScanCardShardRejectsWrongDigest(t *testing.T) {
	dir := t.TempDir()
	writeShard(t, dir, "003", []cardRecord{
		{Principal: "carol@example.org", URI: "u1.vcf", Card: "BEGIN:VCARD\r\nEND:VCARD\r\n"},
		{Principal: "carol@example.org", End: true, N: 1, Digest: "not the digest"},
	})
	b := openTestBundle(t, dir)
	if err := b.scanCardShard("003", func(principalRecords) error { return nil }); err == nil {
		t.Fatal("a sentinel with a wrong digest was accepted")
	}
}

// TestVerifyShardDetectsDamage covers a bundle damaged in transit rather than
// in export — the one thing the sentinel digests cannot see, because they are
// inside the file being checked.
func TestVerifyShardDetectsDamage(t *testing.T) {
	dir := t.TempDir()
	records := []cardRecord{
		{Principal: "dave@example.org", URI: "u1.vcf", Card: "BEGIN:VCARD\r\nEND:VCARD\r\n"},
	}
	records = append(records, cardRecord{
		Principal: "dave@example.org", End: true, N: 1, Digest: principalDigest(records[:1]),
	})
	writeShard(t, dir, "004", records)

	path := filepath.Join(dir, "cards", "004.ndjson.gz")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	writeManifest(t, dir, map[string]shardDigest{
		"cards/004.ndjson.gz": {SHA256: hex.EncodeToString(sum[:]), Records: len(records)},
	})

	b := openTestBundle(t, dir)
	if err := b.verifyShard("cards", "004"); err != nil {
		t.Fatalf("verifyShard on an intact shard: %v", err)
	}
	if err := os.WriteFile(path, append(raw, 'x'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.verifyShard("cards", "004"); err == nil {
		t.Fatal("a damaged shard passed verification")
	}
}

// TestOpenBundleRequiresExporterVersion: without it the ledger cannot pin the
// serializer, and a SnappyMail upgrade between runs turns the next re-run into a
// rewrite of the whole corpus with no signal that anything is wrong.
func TestOpenBundleRequiresExporterVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"),
		[]byte(`{"shards":256,"compression":"gzip"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openBundle(dir); err == nil {
		t.Fatal("a manifest with no exporterVersion was accepted")
	}
}

func openTestBundle(t *testing.T, dir string) *bundle {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); os.IsNotExist(err) {
		writeManifest(t, dir, nil)
	}
	b, err := openBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeManifest(t *testing.T, dir string, files map[string]shardDigest) {
	t.Helper()
	raw, err := json.Marshal(manifest{
		RunID: "00000000-0000-4000-8000-000000000000", ExportEpoch: 1700000000,
		ExporterVersion: "snappymail-export-contacts/1 vobject/4.5.4",
		Shards:          256, Compression: "gzip", Files: files,
		Partial: true, PartialReason: []string{"principals"},
		// Nested per table, exactly as the PHP exporter writes it; a flat
		// map here parsed the test manifests and refused the real one.
		SourceCounts: map[string]map[string]int64{
			"before": {"rainloop_users": 279151, "rainloop_ab_contacts": 3038470},
			"after":  {"rainloop_users": 279260, "rainloop_ab_contacts": 3038942},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeShard writes a gzip NDJSON shard the way the PHP ShardWriter does.
func writeShard(t *testing.T, dir, shard string, records []cardRecord) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "cards"), 0o700); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	for _, rec := range records {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(zw, "%s\n", line)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cards", shard+".ndjson.gz"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestShardNamesIgnoresStaleFiles: the exporter's --force reuses a directory,
// and a run that exported a subset (--only, or run 2's --changed-since) touches
// only some of the 256 shards. Anything left behind is the previous export's
// data, carrying valid sentinels and a matching exporter version — so a
// directory listing would hand the importer last week's contacts as though they
// were part of this bundle. The manifest is the record of what was written.
func TestShardNamesIgnoresStaleFiles(t *testing.T) {
	dir := t.TempDir()
	for _, shard := range []string{"005", "099"} {
		records := []cardRecord{{Principal: "a@example.org", URI: "u.vcf", Card: "BEGIN:VCARD\r\nEND:VCARD\r\n"}}
		records = append(records, cardRecord{
			Principal: "a@example.org", End: true, N: 1, Digest: principalDigest(records[:1]),
		})
		writeShard(t, dir, shard, records)
	}
	// Only one of the two is this export's.
	writeManifest(t, dir, map[string]shardDigest{"cards/005.ndjson.gz": {SHA256: "x", Records: 2}})

	b := openTestBundle(t, dir)
	got, err := b.shardNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "005" {
		t.Errorf("shardNames = %v, want [005] — 099 is on disk but not in the manifest", got)
	}
}

// TestExecuteReportsUnreadableShardsWithoutHanging is a deadlock regression.
//
// Workers used to return on a shard read error. With one worker and more than
// one bad shard, the producer was then left sending into an unbuffered channel
// with no receiver — a batch job that hangs silently instead of reporting, which
// for a 110,557-principal overnight run is the worst failure available. Workers
// now record and carry on.
//
// The importer is nil on purpose: a shard that cannot be parsed never reaches a
// database call, so this exercises the pool without one.
func TestExecuteReportsUnreadableShardsWithoutHanging(t *testing.T) {
	dir := t.TempDir()
	shards := []string{"010", "011", "012"}
	files := map[string]shardDigest{}
	for _, shard := range shards {
		// Records with no sentinel: a truncated export.
		writeShard(t, dir, shard, []cardRecord{
			{Principal: "a@example.org", URI: "u.vcf", Card: "BEGIN:VCARD\r\nEND:VCARD\r\n"},
		})
		files["cards/"+shard+".ndjson.gz"] = shardDigest{SHA256: "x", Records: 1}
	}
	writeManifest(t, dir, files)

	run, err := newImportRun(openTestBundle(t, dir), nil, importOptions{workers: 1, resume: true})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- run.execute(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute returned %v; shard errors belong in the report, not the return", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("execute did not return: the worker pool deadlocked")
	}
	if len(run.shardErrors) != len(shards) {
		t.Errorf("recorded %d shard errors, want %d — a skipped shard silently drops every principal in it",
			len(run.shardErrors), len(shards))
	}
}
