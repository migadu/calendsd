package dav

import (
	"net/http"
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
	return true, nil
}
