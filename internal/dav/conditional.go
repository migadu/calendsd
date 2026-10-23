package dav

import (
	"context"
	"errors"
)

// ErrETagMismatch is returned by a conditional write whose expected ETag no
// longer matches the stored one: another writer got in between the read and
// the write. Backends return it from their conditional update; the core PUT
// turns it into a 412 and the scheduling write-back retries from a fresh
// read (audit 4.3).
var ErrETagMismatch = errors.New("etag mismatch: the resource was modified concurrently")

// ConditionalPutter is implemented by File nodes that can replace their
// content only if the stored ETag still equals expectedETag (quoted, as
// served). The core PUT uses it to make If-Match atomic: the precondition
// check and the write used to be two separate steps with no lock between
// them, so a PUT racing another writer could pass If-Match and still
// overwrite the other write.
type ConditionalPutter interface {
	PutIfMatch(ctx context.Context, data []byte, expectedETag string) (etag string, err error)
}
