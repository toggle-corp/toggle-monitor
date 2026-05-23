package templates

import "context"

// NavMeta is the per-request nav-bar state — currently just the
// total count of open issues. The web Server stuffs it into ctx
// for operator routes; Layout reads it back out when rendering the
// "Issues" nav link.
type NavMeta struct {
	IssueCount int
}

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
