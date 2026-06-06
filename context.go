package allstak

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// Context keys. Using a private type prevents key collisions with any
// other package that might also use context values. This is idiomatic Go.
type ctxKey int

const (
	ctxKeyUser ctxKey = iota
	ctxKeyRequestID
	ctxKeySpan
	ctxKeyRequest
	ctxKeyState
)

// requestState is a per-request mutable bag shared across middleware
// layers. The outer AllStak middleware installs one on the context at
// the top of the request; downstream helpers like WithUser mutate this
// state rather than creating new child contexts so that the outer
// middleware can observe the final user at defer time (e.g. when it
// captures a panic or records the inbound HTTP event).
//
// This pattern solves the classic "middleware ordering" problem where
// an auth middleware downstream of a capture middleware enriches the
// context with a user, but the capture middleware's deferred panic
// handler is still holding the original pre-auth context by value.
type requestState struct {
	user *User

	// crumbs is the request-scoped breadcrumb ring buffer. It is allocated
	// lazily (on the first crumb) via crumbsOnce so requests that never emit a
	// breadcrumb pay nothing. Shared across all middleware layers and the
	// outbound RoundTripper / DB callbacks because the bag is a pointer.
	crumbs     *breadcrumbBuffer
	crumbsOnce sync.Once
}

// newRequestState allocates a fresh state bag for one request.
func newRequestState() *requestState { return &requestState{} }

// stateFromContext returns the request state bag, or nil if none is
// installed (e.g. the caller is using the SDK without the middleware).
func stateFromContext(ctx context.Context) *requestState {
	if s, ok := ctx.Value(ctxKeyState).(*requestState); ok {
		return s
	}
	return nil
}

// withRequestState installs a fresh state bag on the context. Called
// exactly once per request by the outer middleware.
func withRequestState(ctx context.Context, s *requestState) context.Context {
	return context.WithValue(ctx, ctxKeyState, s)
}

// WithRequestState is the integration-facing helper for framework
// middlewares (Gin, Echo, etc.) that live in their own modules. It
// installs a fresh request-state bag on the context so downstream
// WithUser calls propagate back up to the capture layer.
func WithRequestState(ctx context.Context) context.Context {
	return withRequestState(ctx, newRequestState())
}

// User identifies the currently-authenticated principal. This struct is
// what middleware attaches to the context so downstream captures can
// stamp `user.id`, `user.email`, `user.ip` onto error events.
type User struct {
	ID    string
	Email string
	IP    string
}

// WithUser attaches a user to the request-scoped state. If the SDK
// middleware installed a mutable state bag (the common case), this
// function writes to it and returns the same context — so any outer
// middleware that still holds the original context object will observe
// the user at defer time. Without a state bag, it falls back to a
// standard context.WithValue for library consumers that use the capture
// APIs without the middleware.
func WithUser(ctx context.Context, u *User) context.Context {
	if s := stateFromContext(ctx); s != nil {
		s.user = u
		return ctx
	}
	return context.WithValue(ctx, ctxKeyUser, u)
}

// UserFromContext returns the user attached to the context. It first
// checks the mutable request-state bag (so late-arriving users from
// auth middleware are visible), then falls back to the legacy
// context.WithValue key.
func UserFromContext(ctx context.Context) *User {
	if s := stateFromContext(ctx); s != nil && s.user != nil {
		return s.user
	}
	if u, ok := ctx.Value(ctxKeyUser).(*User); ok {
		return u
	}
	return nil
}

// WithRequestID attaches a request ID (typically generated once per
// inbound request) to the context so downstream log and error payloads
// can be correlated.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// RequestIDFromContext returns the request ID, or "" if not set.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// SpanContext captures the minimum span metadata we thread through the
// context for trace propagation. It is intentionally not the same type as
// SpanItem — that is the wire format, this is the in-flight state.
type SpanContext struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	// Sampled is the W3C trace-flags sampled decision for this trace. When a
	// span context is created without an explicit decision (the legacy path,
	// e.g. integrations calling WithContextSpan or an inbound traceparent
	// adopted from upstream) it defaults to true so propagation matches the
	// historical "-01" behavior. StartSpan sets it from TracesSampleRate.
	Sampled bool
}

func withSpan(ctx context.Context, sc *SpanContext) context.Context {
	return context.WithValue(ctx, ctxKeySpan, sc)
}

// WithContextSpan is an integration-facing helper that attaches
// trace/span/parentSpan IDs to the context without forcing integrations
// to import the unexported SpanContext type. Framework middlewares call
// this after generating IDs in order to propagate trace context.
func WithContextSpan(ctx context.Context, traceID, spanID, parentSpanID string) context.Context {
	return withSpan(ctx, &SpanContext{
		TraceID:      traceID,
		SpanID:       spanID,
		ParentSpanID: parentSpanID,
		// Integrations adopt upstream/inbound trace decisions; default to
		// sampled so propagation matches the prior hardcoded "-01" behavior.
		Sampled: true,
	})
}

// SpanFromContext returns the current span context, or nil if none.
// Integrations (middleware, DB hooks, outbound HTTP) use this to stamp
// their own events with the right trace IDs.
func SpanFromContext(ctx context.Context) *SpanContext {
	if sc, ok := ctx.Value(ctxKeySpan).(*SpanContext); ok {
		return sc
	}
	return nil
}

// TraceFromContext returns the (traceId, spanId) pair for the current
// context. Both are empty strings if there is no active span.
func TraceFromContext(ctx context.Context) (string, string) {
	if sc := SpanFromContext(ctx); sc != nil {
		return sc.TraceID, sc.SpanID
	}
	return "", ""
}

// RequestInfo is the per-request metadata stored on the context so that
// any error captured during the request can be enriched with the HTTP
// method, path, host, and user agent.
type RequestInfo struct {
	Method    string
	Path      string
	Host      string
	UserAgent string
}

// WithRequestInfo attaches request metadata to the context. Called by the
// HTTP middleware at the top of each inbound request.
func WithRequestInfo(ctx context.Context, info *RequestInfo) context.Context {
	return context.WithValue(ctx, ctxKeyRequest, info)
}

// RequestInfoFromContext returns the stashed request info, or nil.
func RequestInfoFromContext(ctx context.Context) *RequestInfo {
	if r, ok := ctx.Value(ctxKeyRequest).(*RequestInfo); ok {
		return r
	}
	return nil
}

// enrichFromContext copies user/request/trace metadata from the context
// onto an ErrorPayload. This is called by all high-level capture helpers
// so integrations don't have to remember every field name.
func (c *Client) enrichFromContext(ctx context.Context, p *ErrorPayload) {
	if ctx == nil {
		return
	}
	if u := UserFromContext(ctx); u != nil {
		p.User = &UserContext{ID: u.ID, Email: u.Email, IP: u.IP}
	}
	if ri := RequestInfoFromContext(ctx); ri != nil {
		p.RequestContext = &ReqContext{
			Method:    ri.Method,
			Path:      ri.Path,
			Host:      ri.Host,
			UserAgent: ri.UserAgent,
		}
	}
	if sc := SpanFromContext(ctx); sc != nil {
		p.TraceID = sc.TraceID
		p.SpanID = sc.SpanID
		p.ParentSpanID = sc.ParentSpanID
	}
	if rid := RequestIDFromContext(ctx); rid != "" {
		p.RequestID = rid
	}
	// Attach the request-scoped breadcrumb trail so a captured error carries
	// the http/db/log activity that led up to it. Caller-supplied crumbs win:
	// integrations that already populated Breadcrumbs (rare) keep theirs.
	if len(p.Breadcrumbs) == 0 {
		if crumbs := crumbsFromContext(ctx); crumbs != nil {
			p.Breadcrumbs = crumbs.snapshot()
		}
	}
	if c.cfg.ServiceName != "" {
		if p.Metadata == nil {
			p.Metadata = make(map[string]any, 1)
		}
		p.Metadata["service"] = c.cfg.ServiceName
	}
	// Phase 2 — v2 ingest contract.
	if p.SDKName == "" {
		p.SDKName = "allstak-go"
	}
	if p.SDKVersion == "" {
		p.SDKVersion = sdkVersion
	}
	if p.Platform == "" {
		p.Platform = "go"
	}
	if p.Release == "" {
		p.Release = c.cfg.Release
	}
	if p.Environment == "" {
		p.Environment = c.cfg.Environment
	}
	if p.Dist == "" {
		p.Dist = c.cfg.Dist
	}
}

// ── ID generation ─────────────────────────────────────────────────────────

// NewTraceID returns a 32-character hex trace ID (128 bits of entropy).
// This matches the W3C traceparent format and is wide enough to make
// collisions astronomically unlikely across distributed systems.
func NewTraceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	if allZeros(id) {
		return "1" + id[1:]
	}
	return id
}

// NewSpanID returns a 16-character hex span ID (64 bits of entropy).
// Smaller than a trace ID because span IDs only need to be unique within
// a single trace.
func NewSpanID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	if allZeros(id) {
		return "1" + id[1:]
	}
	return id
}
