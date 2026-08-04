package dav

import "context"

// principalState carries the authenticated principal — and the request-scoped
// submission credential used to send mail as them — through one request. It is
// a mutable holder injected by ServeHTTP before the beforeMethod hooks run, so
// the auth plugin can set them and later hooks can read them — per request,
// unlike sabre's per-request plugin instances.
type principalState struct {
	principal string
	// secret is the plaintext credential the principal presented (the Basic
	// password), stashed by the auth plugin so iMIP can authenticate to the mail
	// submission server as the logged-in user. Request-scoped only: never
	// logged, never persisted, dropped when the request ends. Empty for logins
	// that carry no reusable plaintext (Digest, fixed-principal test logins).
	secret string
}

type principalKey struct{}

// withPrincipalState injects an empty principal holder.
func withPrincipalState(ctx context.Context) context.Context {
	return context.WithValue(ctx, principalKey{}, &principalState{})
}

// SetPrincipal records the authenticated principal for this request.
func SetPrincipal(ctx context.Context, principal string) {
	if s, ok := ctx.Value(principalKey{}).(*principalState); ok {
		s.principal = principal
	}
}

// Principal returns the authenticated principal path for this request, or
// "" when unauthenticated.
func Principal(ctx context.Context) string {
	if s, ok := ctx.Value(principalKey{}).(*principalState); ok {
		return s.principal
	}
	return ""
}

// SetSubmissionSecret records the plaintext credential the authenticated
// principal presented, so send-as-logged-in-user delivery (iMIP) can
// authenticate to the mail submission server as them. It lives only in the
// request-scoped holder and must never be logged or persisted. A no-op when no
// holder is present.
func SetSubmissionSecret(ctx context.Context, secret string) {
	if s, ok := ctx.Value(principalKey{}).(*principalState); ok {
		s.secret = secret
	}
}

// SubmissionSecret returns the credential set by SetSubmissionSecret, or "" if
// none was stashed — an unauthenticated request, or a login mechanism that
// carries no reusable plaintext (Digest, fixed-principal test logins).
func SubmissionSecret(ctx context.Context) string {
	if s, ok := ctx.Value(principalKey{}).(*principalState); ok {
		return s.secret
	}
	return ""
}

// ContextWithPrincipal returns a context carrying principal as the
// authenticated principal, for server-internal callers that act on a
// principal's behalf outside an HTTP request. The WebDAV-Push worker uses it
// to re-check {DAV:}read via the ACL plugin before dispatching a notification,
// reproducing the principal state ServeHTTP installs on a real request.
func ContextWithPrincipal(ctx context.Context, principal string) context.Context {
	ctx = withPrincipalState(ctx)
	SetPrincipal(ctx, principal)
	return ctx
}
