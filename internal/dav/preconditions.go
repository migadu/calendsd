package dav

import (
	"net/http"
	"regexp"
	"strings"
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
		node, err = s.Tree.NodeForPath(path)
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
				item = strings.Trim(item, " ")
				// The \" prefix strip works around an Evolution bug,
				// like sabre.
				if item == etag || strings.ReplaceAll(item, `\"`, `"`) == etag {
					match = true
				}
			}
			if !match {
				if etag != "" {
					w.Header().Set("ETag", etag)
				}
				return false, ErrPreconditionFailed("An If-Match header was specified, but none of the specified ETags matched.")
			}
		}
	}

	if ifNoneMatch := r.Header.Get("If-None-Match"); ifNoneMatch != "" {
		exists := node != nil
		if !exists {
			node, err = s.Tree.NodeForPath(path)
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
					if strings.Trim(item, " ") == etag && etag != "" {
						match = true
					}
				}
			}
			if match {
				if etag != "" {
					w.Header().Set("ETag", etag)
				}
				if r.Method == "GET" {
					w.WriteHeader(http.StatusNotModified)
					return false, nil
				}
				return false, ErrPreconditionFailed("An If-None-Match header was specified, but the ETag matched (or * was specified).")
			}
		}
		return true, nil // If-None-Match suppresses If-Modified-Since
	}

	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if date, parseErr := http.ParseTime(ims); parseErr == nil {
			if node == nil {
				if node, err = s.Tree.NodeForPath(path); err != nil {
					return false, err
				}
			}
			if lastMod := node.LastModified(); !lastMod.IsZero() && !lastMod.After(date) {
				w.Header().Set("Last-Modified", lastMod.UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusNotModified)
				return false, nil
			}
		}
	}

	if ius := r.Header.Get("If-Unmodified-Since"); ius != "" {
		if date, parseErr := http.ParseTime(ius); parseErr == nil {
			if node == nil {
				if node, err = s.Tree.NodeForPath(path); err != nil {
					return false, err
				}
			}
			if lastMod := node.LastModified(); !lastMod.IsZero() && lastMod.After(date) {
				return false, ErrPreconditionFailed("An If-Unmodified-Since header was specified, but the entity has been changed since the specified date.")
			}
		}
	}

	return true, s.checkIfConditions(r, path)
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
	ValidateTokens(conditions []IfCondition)
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
			tv.ValidateTokens(conditions)
		}
	}

nextCondition:
	for _, cond := range conditions {
		for _, token := range cond.Tokens {
			tokenValid := token.Valid || token.Token == ""
			etagValid := token.ETag == ""
			if token.ETag != "" && tokenValid {
				if node, err := s.Tree.NodeForPath(cond.URI); err == nil {
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
