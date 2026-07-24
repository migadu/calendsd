package mock

import (
	"strconv"

	"calserver/internal/dav"
)

// SimpleFile is a read-only file: deletion and renaming are forbidden, like
// sabre's SimpleFile (which inherits DAV\Node's forbidding defaults).
type SimpleFile struct {
	File
}

// NewSimpleFile builds a read-only file.
func NewSimpleFile(name, contents string) *SimpleFile {
	return &SimpleFile{File: *NewFile(name, contents)}
}

func (f *SimpleFile) Delete() error {
	return dav.ErrForbidden("Permission denied to delete node")
}

func (f *SimpleFile) SetName(string) error {
	return dav.ErrForbidden("Permission denied to rename file")
}

// ChangeEntry is one recorded change batch of a SyncCollection.
type ChangeEntry struct {
	Token    int
	Added    []string
	Modified []string
	Deleted  []string
}

// SyncCollection is an in-memory sync-capable collection. Port of sabre's
// test MockSyncCollection, including its quirky limit accounting (only
// modified+deleted count toward the limit).
type SyncCollection struct {
	Collection
	Token     int
	ChangeLog []ChangeEntry
}

// NewSyncCollection builds a sync collection.
func NewSyncCollection(name string, children ...dav.Node) *SyncCollection {
	sc := &SyncCollection{}
	sc.Collection.name = name
	for _, child := range children {
		sc.Attach(child)
	}
	return sc
}

// AddChange records a change batch and bumps the token.
func (sc *SyncCollection) AddChange(added, modified, deleted []string) {
	sc.Token++
	sc.ChangeLog = append(sc.ChangeLog, ChangeEntry{
		Token: sc.Token, Added: added, Modified: modified, Deleted: deleted,
	})
}

func (sc *SyncCollection) SyncToken() string {
	if sc.Token == 0 {
		return ""
	}
	return strconv.Itoa(sc.Token)
}

func (sc *SyncCollection) Changes(syncToken string, syncLevel, limit int) (*dav.ChangeSet, error) {
	if syncToken == "" {
		children, err := sc.Children()
		if err != nil {
			return nil, err
		}
		out := &dav.ChangeSet{SyncToken: strconv.Itoa(sc.Token)}
		for _, c := range children {
			out.Added = append(out.Added, c.Name())
		}
		return out, nil
	}
	since, err := strconv.Atoi(syncToken)
	if err != nil || sc.Token == 0 {
		return nil, nil // invalid token
	}

	out := &dav.ChangeSet{SyncToken: strconv.Itoa(sc.Token)}
	for _, entry := range sc.ChangeLog {
		if entry.Token <= since {
			continue
		}
		out.Added = append(out.Added, entry.Added...)
		out.Modified = append(out.Modified, entry.Modified...)
		out.Deleted = append(out.Deleted, entry.Deleted...)

		if limit > 0 {
			// Sabre's mock counts only modified+deleted toward the limit
			// and truncates in that order; ported verbatim.
			left := limit - (len(out.Modified) + len(out.Deleted))
			if left > 0 {
				continue
			}
			if left == 0 {
				break
			}
			out.Truncated = true
			// PHP's array_slice tolerates a negative length that exceeds
			// the slice; Go's slice bound would panic, so clamp to 0 when
			// deletions alone already exceed the limit.
			out.Modified = out.Modified[:max(0, len(out.Modified)+left)]
			left = limit - (len(out.Modified) + len(out.Deleted))
			if left == 0 {
				break
			}
			if left < 0 {
				out.Deleted = out.Deleted[:max(0, len(out.Deleted)+left)]
			}
			break
		}
	}
	return out, nil
}
