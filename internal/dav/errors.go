package dav

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Error is a protocol error carrying the HTTP status and an exception
// identity. Error bodies are {DAV:}error documents with a
// {http://sabredav.org/ns}exception class name, as sabre/dav sent them;
// clients (and the corpus) can observe those names, so they are kept.
type Error struct {
	Status int
	// Exception is the class name sent as {http://sabredav.org/ns}exception,
	// e.g. "Sabre\DAV\Exception\NotFound".
	Exception string
	Message   string
	// Precondition names protocol precondition/postcondition elements
	// (e.g. {urn:ietf:params:xml:ns:caldav}valid-calendar-data), serialized
	// as children of {DAV:}error alongside the exception.
	Precondition []xml.Name
	// RetryAfter, when set, is emitted as a Retry-After header (429s).
	RetryAfter time.Duration
	// cause is the underlying error of a 500 wrapped by AsError. It is
	// logged, never sent: driver and file-system messages name tables,
	// hosts and paths (audit 6.3).
	cause error
}

// Cause returns the wrapped internal error of a 500, or nil.
func (e *Error) Cause() error { return e.cause }

// ErrTooManyRequests is a 429 carrying a Retry-After; the auth plugin returns
// it for a throttled login (audit 5.8). Its exception name has no sabre/dav
// counterpart.
func ErrTooManyRequests(retryAfter time.Duration, format string, args ...any) *Error {
	e := davError(http.StatusTooManyRequests, "TooManyRequests", format, args...)
	e.RetryAfter = retryAfter
	return e
}

func (e *Error) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

func davError(status int, exception, format string, args ...any) *Error {
	return &Error{
		Status:    status,
		Exception: `Sabre\DAV\Exception\` + exception,
		Message:   fmt.Sprintf(format, args...),
	}
}

// IsNotFound reports whether err is a 404 protocol error.
func IsNotFound(err error) bool {
	var de *Error
	return errors.As(err, &de) && de.Status == http.StatusNotFound
}

// Found turns the error of a lookup into an existence check: nil means
// found, a 404 means not found, and any other error is returned.
func Found(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	if IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// The constructors below pair each status with its sabre/dav exception name.

func ErrNotFound(format string, args ...any) *Error {
	return davError(http.StatusNotFound, "NotFound", format, args...)
}

func ErrForbidden(format string, args ...any) *Error {
	return davError(http.StatusForbidden, "Forbidden", format, args...)
}

func ErrConflict(format string, args ...any) *Error {
	return davError(http.StatusConflict, "Conflict", format, args...)
}

func ErrBadRequest(format string, args ...any) *Error {
	return davError(http.StatusBadRequest, "BadRequest", format, args...)
}

func ErrPreconditionFailed(format string, args ...any) *Error {
	return davError(http.StatusPreconditionFailed, "PreconditionFailed", format, args...)
}

func ErrUnsupportedMediaType(format string, args ...any) *Error {
	return davError(http.StatusUnsupportedMediaType, "UnsupportedMediaType", format, args...)
}

func ErrMethodNotAllowed(format string, args ...any) *Error {
	return davError(http.StatusMethodNotAllowed, "MethodNotAllowed", format, args...)
}

func ErrNotImplemented(format string, args ...any) *Error {
	return davError(http.StatusNotImplemented, "NotImplemented", format, args...)
}

func ErrNotAuthenticated(format string, args ...any) *Error {
	return davError(http.StatusUnauthorized, "NotAuthenticated", format, args...)
}

func ErrReportNotSupported() *Error {
	return davError(http.StatusUnsupportedMediaType, "ReportNotSupported", "The report you requested is not supported")
}

// AsError extracts a protocol *Error, wrapping unknown errors as 500s.
func AsError(err error) *Error {
	var de *Error
	if errors.As(err, &de) {
		return de
	}
	// A body exceeding Server.MaxRequestBodySize surfaces here as a
	// *http.MaxBytesError; report it as 413 rather than a generic 500.
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return davError(http.StatusRequestEntityTooLarge, "RequestEntityTooLarge",
			"The request body exceeds the maximum allowed size")
	}
	return &Error{
		Status:    http.StatusInternalServerError,
		Exception: `Exception`,
		Message:   "Internal Server Error",
		cause:     err,
	}
}
