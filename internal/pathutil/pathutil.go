// Package pathutil holds the one path helper shared by every DAV package.
package pathutil

import "strings"

// Split splits a slash-separated DAV path or principal URI into its parent
// and final segment: "principals/alice" → ("principals", "alice"). A path
// without a slash is all base: "root" → ("", "root"). Trailing slashes are
// not trimmed — callers pass canonical paths.
func Split(path string) (parent, base string) {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return "", path
	}
	return path[:i], path[i+1:]
}
