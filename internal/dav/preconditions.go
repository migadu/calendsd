package dav

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// checkPreconditions evaluates If-Match / If-None-Match / If-Modified-Since /
// If-Unmodified-Since before method dispatch. Port of sabre's
// Server::checkPreconditions. proceed=false with nil error means a 304 was
// written.
func (s *Server) checkPreconditions(w http.ResponseWriter, r *http.Request) (proceed bool, err error) {
	path, pathErr := s.Path(r)
	if pathErr != nil {
		// Leave URI errors to the method handlers.
		return true, nil
	}

	var node Node
	var etag string

	nodeETag := func() string {
		if file, isFile := node.(File); isFile {
			return file.ETag()
		}
		return ""
	}

	if ifMatch := r.Header.Get("If-Match"); ifMatch != "" {
		node, err = s.Tree.NodeForPath(r.Context(), path)
		if err != nil {
			if AsError(err).Status == http.StatusNotFound {
				return false, ErrPreconditionFailed("An If-Match header was specified and the resource did not exist")
			}
			return false, err
		}
		if ifMatch != "*" {
			etag = nodeETag()
			match := false
			for _, item := range strings.Split(ifMatch, ",") {
				if etagMatches(item, etag) {
					match = true
				}
			}
			if !match {
				if etag != "" {
					w.Header().Set("ETag", etag)
				}
				// A save that 412s here on a single-writer server means the
				// client's If-Match (from its GET) and the node's current ETag
				// disagree with no intervening write — log both, byte-for-byte,
				// so the cause (etag format, stale client copy) is not a guess.
				slog.Debug("If-Match precondition failed",
					"method", r.Method, "path", path,
					"if_match", ifMatch, "node_etag", etag,
					"if_match_bytes", []byte(ifMatch), "node_etag_bytes", []byte(etag))
				return false, ErrPreconditionFailed("An If-Match header was specified, but none of the specified ETags matched.")
			}
		}
	}

	if ifNoneMatch := r.Header.Get("If-None-Match"); ifNoneMatch != "" {
		exists := node != nil
		if !exists {
			node, err = s.Tree.NodeForPath(r.Context(), path)
			switch {
			case err == nil:
				exists = true
			case AsError(err).Status == http.StatusNotFound:
				exists = false
			default:
				return false, err
			}
		}
		if exists {
			match := ifNoneMatch == "*"
			if !match {
				etag = nodeETag()
				for _, item := range strings.Split(ifNoneMatch, ",") {
					if etagMatches(item, etag) {
						match = true
					}
				}
			}
			if match {
				if etag != "" {
					w.Header().Set("ETag", etag)
				}
				if r.Method == http.MethodGet || r.Method == http.MethodHead {
					w.WriteHeader(http.StatusNotModified)
					return false, nil
				}
				return false, ErrPreconditionFailed("An If-None-Match header was specified, but the ETag matched (or * was specified).")
			}
		}
	}

	// The date conditions need the target; a missing one has no
	// modification date, so they do not apply (the method handler decides
	// what a missing target means).
	lastModified := func() (time.Time, error) {
		if node == nil {
			node, err = s.Tree.NodeForPath(r.Context(), path)
			if err != nil {
				if AsError(err).Status == http.StatusNotFound {
					return time.Time{}, nil
				}
				return time.Time{}, err
			}
		}
		return node.LastModified(), nil
	}

	// If-Modified-Since applies to GET and HEAD only, and If-None-Match
	// overrides it (RFC 7232 §3.3).
	if ims := r.Header.Get("If-Modified-Since"); ims != "" && r.Header.Get("If-None-Match") == "" &&
		(r.Method == http.MethodGet || r.Method == http.MethodHead) {
		if date, parseErr := http.ParseTime(ims); parseErr == nil {
			lastMod, err := lastModified()
			if err != nil {
				return false, err
			}
			if !lastMod.IsZero() && !lastMod.After(date) {
				w.Header().Set("Last-Modified", lastMod.UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusNotModified)
				return false, nil
			}
		}
	}

	// If-Match overrides If-Unmodified-Since (RFC 7232 §3.4).
	if ius := r.Header.Get("If-Unmodified-Since"); ius != "" && r.Header.Get("If-Match") == "" {
		if date, parseErr := http.ParseTime(ius); parseErr == nil {
			lastMod, err := lastModified()
			if err != nil {
				return false, err
			}
			if !lastMod.IsZero() && lastMod.After(date) {
				return false, ErrPreconditionFailed("An If-Unmodified-Since header was specified, but the entity has been changed since the specified date.")
			}
		}
	}

	return true, s.checkIfConditions(r, path)
}

// etagMatches reports whether a client-supplied entity-tag (one comma-separated
// item of an If-Match / If-None-Match header) identifies the node's current
// ETag. Both sides are normalized first (see normalizeETag). An empty node
// ETag never matches.
func etagMatches(clientTag, nodeETag string) bool {
	if nodeETag == "" {
		return false
	}
	return normalizeETag(clientTag) == normalizeETag(nodeETag)
}

// normalizeETag canonicalizes an entity-tag for comparison. It drops a leading
// weak-validator "W/" and unescapes an \"-escaped tag.
//
// The W/ strip is what lets conditional writes survive a compressing reverse
// proxy: an intermediary that gzips a GET (Caddy's `encode`, nginx `gzip`, …)
// rewrites the strong ETag it forwards into a weak one, because the compressed
// body is a different representation (RFC 7232 §2.3.1). A browser app then reads
// `W/"x"` off that GET and replays it as `If-Match: W/"x"` on its PUT — which,
// under the strong comparison If-Match normally requires, could never match the
// origin's strong `"x"`, so every save 412s. The underlying entity is the same,
// so matching them is the correct intent for a whole-body conditional write.
// The \" unescape works around an Evolution bug, like sabre.
func normalizeETag(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == 'W' || s[0] == 'w') && s[1] == '/' {
		s = s[2:]
	}
	return strings.ReplaceAll(s, `\"`, `"`)
}

// IfCondition is one parsed tagged-list of an If header (RFC 4918 §10.4).
type IfCondition struct {
	URI    string
	Tokens []IfToken
}

// IfToken is one (state-token + etag) clause of a condition.
type IfToken struct {
	Negate bool
	Token  string
	ETag   string
	// Valid is set by TokenValidator plugins (e.g. the sync plugin).
	Valid bool
}

// TokenValidator lets plugins mark state tokens in If conditions as valid.
// Replaces sabre's validateTokens event.
type TokenValidator interface {
	ValidateTokens(ctx context.Context, conditions []IfCondition)
}

// checkIfConditions evaluates the If header: for each condition, at least
// one clause must hold. Port of the tail of sabre's checkPreconditions.
func (s *Server) checkIfConditions(r *http.Request, path string) error {
	conditions := s.parseIfConditions(r, path)
	if len(conditions) == 0 {
		return nil
	}
	for _, p := range s.plugins {
		if tv, ok := p.(TokenValidator); ok {
			tv.ValidateTokens(r.Context(), conditions)
		}
	}

nextCondition:
	for _, cond := range conditions {
		for _, token := range cond.Tokens {
			tokenValid := token.Valid || token.Token == ""
			etagValid := token.ETag == ""
			if token.ETag != "" && tokenValid {
				if node, err := s.Tree.NodeForPath(r.Context(), cond.URI); err == nil {
					if file, isFile := node.(File); isFile && file.ETag() == token.ETag {
						etagValid = true
					}
				}
			}
			if (tokenValid && etagValid) != token.Negate {
				continue nextCondition
			}
		}
		return ErrPreconditionFailed("Failed to find a valid token/etag combination for %s", cond.URI)
	}
	return nil
}

// ifHeaderRe is sabre's getIfConditions regex, verbatim.
var ifHeaderRe = regexp.MustCompile(`(?i)(?:<(?P<uri>.*?)>\s)?\((?P<not>Not\s)?(?:<(?P<token>[^>]*)>)?(?:\s?)(?:\[(?P<etag>[^\]]*)\])?\)`)

func (s *Server) parseIfConditions(r *http.Request, requestPath string) []IfCondition {
	header := r.Header.Get("If")
	if header == "" {
		return nil
	}
	var conditions []IfCondition
	for _, m := range ifHeaderRe.FindAllStringSubmatch(header, -1) {
		uri, not, token, etag := m[1], m[2], m[3], m[4]
		ifToken := IfToken{Negate: not != "", Token: token, ETag: etag}
		if uri == "" && len(conditions) > 0 {
			last := &conditions[len(conditions)-1]
			last.Tokens = append(last.Tokens, ifToken)
			continue
		}
		realURI := requestPath
		if uri != "" {
			if stripped, err := s.stripBase(uri); err == nil {
				realURI = stripped
			}
		}
		conditions = append(conditions, IfCondition{URI: realURI, Tokens: []IfToken{ifToken}})
	}
	return conditions
}
