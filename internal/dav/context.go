package dav

import "context"

// principalState carries the authenticated principal through one request.
// It is a mutable holder injected by ServeHTTP before the beforeMethod
// hooks run, so the auth plugin can set it and later hooks can read it —
// per request, unlike sabre's per-request plugin instances.
type principalState struct {
	principal string
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
