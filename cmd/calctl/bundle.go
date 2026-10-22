package main

// Reader for the export bundle produced by scripts/snappymail-export-contacts.php
// in the ansible repo (docs/snappymail-calserver-contacts.md § The bundle).
//
// The bundle is deliberately a file and not a pipe: produced once, checksummed,
// then read as many times as the import needs. That is what lets the import be
// re-run without re-running the export, lets the export be validated on its own
// terms, and makes "which bytes did we actually write" answerable three weeks
// later. A pipeline whose only output is rows in the destination answers none
// of those.
//
//	manifest.json                 run id, epoch, versions, per-shard digests
//	principals.tsv                principal, records, digest — one line each
//	cards/NNN.ndjson.gz           card records + a sentinel per principal
//	frecency/NNN.ndjson.gz        seeded usage counts
//	rejects.ndjson                contacts the exporter refused, with reasons
//
// NNN is the first byte of sha256(principal) as three decimal digits, so all of
// a principal's records live in one shard and a shard is independently
// importable.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// manifest is bundle/manifest.json. Its version fields are load-bearing: the
// importer refuses a bundle whose exporter differs from the one a principal's
// ledger row recorded, because a serializer change makes every digest differ
// for reasons that are not data.
type manifest struct {
	RunID       string `json:"runId"`
	ExportEpoch int64  `json:"exportEpoch"`
	// ExporterVersion identifies the exporter AND the vCard serializer behind
	// it, e.g. "snappymail-export-contacts/1 snappymail/2.38.2 vobject/4.5.4".
	ExporterVersion string                 `json:"exporterVersion"`
	UIDNamespace    string                 `json:"uidNamespace"`
	Shards          int                    `json:"shards"`
	Compression     string                 `json:"compression"`
	Principals      int                    `json:"principals"`
	Cards           int                    `json:"cards"`
	FrecencyRows    int                    `json:"frecencyRows"`
	Rejected        int                    `json:"rejected"`
	Files           map[string]shardDigest `json:"files"`
	// Partial says the bundle is not the whole corpus, with PartialReason
	// naming the selectors that narrowed it. Run 2 of the cutover is always
	// partial by design, so this is informational, not a warning.
	Partial       bool     `json:"partial,omitempty"`
	PartialReason []string `json:"partialReason,omitempty"`
	// SourceCounts records the SnappyMail row counts before and after the
	// export, proving it wrote nothing to the store it read. Shaped as the
	// exporter writes it: {"before": {table: rows}, "after": {table: rows}}.
	// Informational here — nothing in the importer acts on it — but the type
	// has to match or the manifest does not parse at all, which is how run 1's
	// bundle was refused on 2026-09-24.
	SourceCounts map[string]map[string]int64 `json:"sourceCounts,omitempty"`
}

type shardDigest struct {
	SHA256  string `json:"sha256"`
	Records int    `json:"records"`
	Bytes   int64  `json:"bytes"`
}

// cardRecord is one line of a cards shard. A line with End set is the
// per-principal sentinel that closes it.
type cardRecord struct {
	Principal string `json:"p"`
	URI       string `json:"uri,omitempty"`
	UID       string `json:"uid,omitempty"`
	MTime     int64  `json:"mtime,omitempty"`
	// Path and UIDSource are diagnostics the importer never acts on: which of
	// the two mappings produced the card, and whether its uid came from the
	// source row or had to be derived. Both halves of each split are large
	// enough that telling them apart afterwards is worth four bytes a record.
	Path      string `json:"path,omitempty"`   // "jcard" | "rebuild"
	UIDSource string `json:"uidsrc,omitempty"` // "source" | "derived"
	Card      string `json:"card,omitempty"`

	End    bool    `json:"end,omitempty"`
	N      int     `json:"n,omitempty"`
	Digest string  `json:"digest,omitempty"`
	Users  []int32 `json:"users,omitempty"`
}

type frecencyRecord struct {
	Principal string `json:"p"`
	Email     string `json:"email"`
	Name      string `json:"name,omitempty"`
	Frec      int    `json:"frec"`
}

// bundle is an opened export bundle.
type bundle struct {
	dir      string
	manifest manifest
}

func openBundle(dir string) (*bundle, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("reading bundle manifest: %w", err)
	}
	b := &bundle{dir: dir}
	if err := json.Unmarshal(raw, &b.manifest); err != nil {
		return nil, fmt.Errorf("parsing bundle manifest: %w", err)
	}
	switch {
	case b.manifest.ExporterVersion == "":
		return nil, fmt.Errorf("bundle manifest has no exporterVersion; the ledger cannot pin the serializer without it")
	case b.manifest.Shards <= 0:
		return nil, fmt.Errorf("bundle manifest declares %d shards", b.manifest.Shards)
	case b.manifest.Compression != "gzip" && b.manifest.Compression != "none":
		return nil, fmt.Errorf("bundle compression %q is not supported (want gzip or none)", b.manifest.Compression)
	}
	return b, nil
}

// shardNames lists the shard numbers to import, as the "NNN" strings the file
// names use.
//
// Taken from the manifest's file list, NOT from the directory, and that
// distinction matters: an exporter run with --force reuses a directory, and a
// run that exported a subset (--only, or run 2's --changed-since) leaves the
// previous run's shards on disk beside the new ones. Those stale files carry
// valid sentinels and a matching exporter version, so a directory listing would
// hand the importer last week's data as though it were this bundle's. The
// manifest is the record of what this export actually wrote.
func (b *bundle) shardNames() ([]string, error) {
	if len(b.manifest.Files) > 0 {
		seen := map[string]bool{}
		var out []string
		for rel := range b.manifest.Files {
			name, ok := strings.CutPrefix(rel, "cards/")
			if !ok {
				continue
			}
			if i := strings.IndexByte(name, '.'); i > 0 {
				name = name[:i]
			}
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		sort.Strings(out)
		return out, nil
	}
	// A manifest with no file list is only produced by hand (and by tests); fall
	// back to the directory rather than importing nothing.
	entries, err := os.ReadDir(filepath.Join(b.dir, "cards"))
	if err != nil {
		return nil, fmt.Errorf("listing bundle shards: %w", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if i := strings.IndexByte(name, '.'); i > 0 {
			out = append(out, name[:i])
		}
	}
	sort.Strings(out)
	return out, nil
}

func (b *bundle) shardPath(kind, shard string) string {
	name := shard + ".ndjson"
	if b.manifest.Compression == "gzip" {
		name += ".gz"
	}
	return filepath.Join(b.dir, kind, name)
}

// openShard opens one shard for reading, transparently decompressing. The
// returned closer closes every layer.
func (b *bundle) openShard(kind, shard string) (io.Reader, io.Closer, error) {
	path := b.shardPath(kind, shard)
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	if b.manifest.Compression != "gzip" {
		return bufio.NewReaderSize(f, 1<<20), f, nil
	}
	zr, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return zr, multiCloser{zr, f}, nil
}

type multiCloser []io.Closer

func (m multiCloser) Close() error {
	var first error
	for _, c := range m {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// verifyShard recomputes a shard file's sha256 and compares it to the
// manifest. Cheap insurance against a truncated copy, and the only check that
// catches a bundle damaged in transit rather than in export.
func (b *bundle) verifyShard(kind, shard string) error {
	rel := kind + "/" + filepath.Base(b.shardPath(kind, shard))
	want, ok := b.manifest.Files[rel]
	if !ok {
		return fmt.Errorf("%s is not in the manifest", rel)
	}
	f, err := os.Open(b.shardPath(kind, shard))
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want.SHA256 {
		return fmt.Errorf("%s: sha256 %s, manifest says %s", rel, got, want.SHA256)
	}
	return nil
}

// readFrecencyShard loads one frecency shard into a per-principal map.
//
// Loaded whole rather than merge-joined against the cards shard, even though
// both are written in principal order: that order is Postgres's, under whatever
// collation the source database uses, and Go compares strings bytewise. A
// merge-join would silently lose rows wherever the two orders disagree. A shard
// is 1/256th of the frecency rows, so reading it into memory costs nothing and
// removes the question.
func (b *bundle) readFrecencyShard(shard string) (map[string][]frecencyRecord, error) {
	r, closer, err := b.openShard("frecency", shard)
	if os.IsNotExist(err) {
		return map[string][]frecencyRecord{}, nil // a bundle may carry no frecency
	}
	if err != nil {
		return nil, err
	}
	defer closer.Close()

	out := map[string][]frecencyRecord{}
	sc := newNDJSONScanner(r)
	for sc.Scan() {
		var rec frecencyRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("frecency shard %s line %d: %w", shard, sc.line, err)
		}
		if rec.Principal == "" || rec.Email == "" || rec.Frec <= 0 {
			continue
		}
		out[rec.Principal] = append(out[rec.Principal], rec)
	}
	return out, sc.Err()
}

// principalRecords is one principal's complete set of card records, closed by
// its sentinel.
type principalRecords struct {
	Principal string
	Cards     []cardRecord
	Digest    string
	Users     []int32
}

// scanCardShard streams a cards shard, calling fn once per principal as its
// sentinel arrives.
//
// Records for a principal are contiguous because the exporter writes principal
// by principal, so this holds one principal in memory at a time rather than a
// whole shard. A principal whose records are present but whose sentinel is
// missing is an export truncated by a crash — reported as an error rather than
// imported short, which is the one failure mode nothing downstream could detect.
func (b *bundle) scanCardShard(shard string, fn func(principalRecords) error) error {
	r, closer, err := b.openShard("cards", shard)
	if err != nil {
		return err
	}
	defer closer.Close()

	var current principalRecords
	seen := map[string]bool{}
	sc := newNDJSONScanner(r)
	for sc.Scan() {
		var rec cardRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return fmt.Errorf("cards shard %s line %d: %w", shard, sc.line, err)
		}
		if rec.Principal == "" {
			return fmt.Errorf("cards shard %s line %d: record has no principal", shard, sc.line)
		}
		if current.Principal != "" && rec.Principal != current.Principal {
			return fmt.Errorf("cards shard %s line %d: %s starts before %s is closed by a sentinel",
				shard, sc.line, rec.Principal, current.Principal)
		}
		if rec.End {
			if current.Principal == "" {
				current.Principal = rec.Principal
			}
			if len(current.Cards) != rec.N {
				return fmt.Errorf("cards shard %s: %s has %d records, sentinel says %d",
					shard, rec.Principal, len(current.Cards), rec.N)
			}
			if got := principalDigest(current.Cards); got != rec.Digest {
				return fmt.Errorf("cards shard %s: %s digest %s, sentinel says %s",
					shard, rec.Principal, got, rec.Digest)
			}
			current.Digest, current.Users = rec.Digest, rec.Users
			if err := fn(current); err != nil {
				return err
			}
			current = principalRecords{}
			continue
		}
		if current.Principal == "" {
			if seen[rec.Principal] {
				return fmt.Errorf("cards shard %s line %d: %s appears in a second block",
					shard, sc.line, rec.Principal)
			}
			seen[rec.Principal] = true
			current.Principal = rec.Principal
		}
		current.Cards = append(current.Cards, rec)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if current.Principal != "" {
		return fmt.Errorf("cards shard %s: %s has %d records and no sentinel — the export was truncated",
			shard, current.Principal, len(current.Cards))
	}
	return nil
}

// principalDigest recomputes the sentinel digest: sha256 over the principal's
// records in emitted order, of uri || NUL || sha256(card) each.
//
// It does three jobs. It detects a bundle truncated by a crash. It lets the
// importer decide "nothing changed for this principal" in one comparison rather
// than N. And it makes "is this the same export?" answerable without diffing
// gigabytes.
func principalDigest(cards []cardRecord) string {
	h := sha256.New()
	for _, c := range cards {
		card := sha256.Sum256([]byte(c.Card))
		h.Write([]byte(c.URI))
		h.Write([]byte{0})
		h.Write([]byte(hex.EncodeToString(card[:])))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ndjsonScanner is bufio.Scanner with a line counter and a buffer large enough
// for the biggest real card (6.97 MB, an embedded photo) plus JSON escaping.
type ndjsonScanner struct {
	*bufio.Scanner
	line int
}

func newNDJSONScanner(r io.Reader) *ndjsonScanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	return &ndjsonScanner{Scanner: sc}
}

func (s *ndjsonScanner) Scan() bool {
	for s.Scanner.Scan() {
		s.line++
		// Bytes, not Text: Text copies the whole line (up to the 64 MiB cap)
		// just to test it for blankness.
		if len(bytes.TrimSpace(s.Bytes())) > 0 {
			return true
		}
	}
	return false
}
