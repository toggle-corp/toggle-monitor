package templates

import (
	"context"
	"strings"
)

// NavMeta is the per-request nav-bar state: the total count of open
// issues, and which top-level section the request lands in. The web
// Server stuffs it into ctx for operator routes; Layout reads it back
// out when rendering the nav.
type NavMeta struct {
	IssueCount int
	// Active is the href of the current section ("/monitors"), matched
	// against the nav links to mark one current. Empty for requests
	// outside the operator nav.
	Active string
}

// navSection maps a request path onto the nav link that owns it, so
// /monitor/<slug> lights up "Monitors" and /alert/<id> lights up
// "Alerts".
func navSection(path string) string {
	switch {
	case path == "/":
		return "/"
	case strings.HasPrefix(path, "/monitors"), strings.HasPrefix(path, "/monitor/"):
		return "/monitors"
	case strings.HasPrefix(path, "/status"):
		return "/status"
	case strings.HasPrefix(path, "/discovery"):
		return "/discovery"
	case strings.HasPrefix(path, "/alerts"), strings.HasPrefix(path, "/alert/"):
		return "/alerts"
	case strings.HasPrefix(path, "/issues"):
		return "/issues"
	default:
		return ""
	}
}

// NavSectionFor is navSection, exported for the Server's nav
// middleware.
func NavSectionFor(path string) string { return navSection(path) }

type navCtxKey struct{}

// WithNav returns ctx with the given NavMeta attached. Operator
// routes call this in a middleware so every Layout-using template
// sees the same metadata.
func WithNav(ctx context.Context, m NavMeta) context.Context {
	return context.WithValue(ctx, navCtxKey{}, m)
}

// NavFromCtx fetches the NavMeta out of ctx, returning a zero
// value when none was attached (the public /status page and tests
// that bypass the middleware).
func NavFromCtx(ctx context.Context) NavMeta {
	if v, ok := ctx.Value(navCtxKey{}).(NavMeta); ok {
		return v
	}
	return NavMeta{}
}
