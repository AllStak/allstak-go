package allstak

import (
	"context"
	"sync"
	"time"
)

// defaultMaxBreadcrumbs is the size of the per-request breadcrumb ring buffer.
// Once it fills, the oldest crumb is evicted to make room for the newest, so
// a captured error always carries the most-recent trail of events leading up
// to it. The value mirrors the common cross-SDK default.
const defaultMaxBreadcrumbs = 100

// breadcrumbBuffer is a bounded, concurrency-safe ring buffer of Breadcrumb
// values. It lives in the request-scoped state bag so every layer touching a
// single request (inbound middleware, the outbound RoundTripper, the GORM
// after-callback, the structured-log helpers) appends to the SAME trail, and
// enrichFromContext snapshots that trail onto any error captured during the
// request.
//
// The buffer is intentionally request-scoped rather than process-global: a
// global trail would interleave unrelated concurrent requests and leak crumbs
// across tenant boundaries. Library consumers that capture outside a request
// (e.g. a background worker) simply get no auto-trail unless they install a
// state bag via WithRequestState / WithBreadcrumbs.
type breadcrumbBuffer struct {
	mu    sync.Mutex
	items []Breadcrumb
	max   int
}

func newBreadcrumbBuffer(max int) *breadcrumbBuffer {
	if max <= 0 {
		max = defaultMaxBreadcrumbs
	}
	return &breadcrumbBuffer{max: max}
}

// add appends a crumb, evicting the oldest when the buffer is full so the
// trail always reflects the most-recent activity.
func (b *breadcrumbBuffer) add(bc Breadcrumb) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) >= b.max {
		// Drop the oldest. A simple shift keeps insertion order without an
		// index-tracking ring; max is small (default 100) so this is cheap and
		// far less error-prone than a manual circular index.
		copy(b.items, b.items[1:])
		b.items[len(b.items)-1] = bc
		return
	}
	b.items = append(b.items, bc)
}

// snapshot returns a defensive copy of the current trail in chronological
// order. The copy is safe to hand to the capture path without further locking.
func (b *breadcrumbBuffer) snapshot() []Breadcrumb {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) == 0 {
		return nil
	}
	out := make([]Breadcrumb, len(b.items))
	copy(out, b.items)
	return out
}

// crumbsFromContext returns the request-scoped breadcrumb buffer, or nil when
// no request-state bag is installed. It is the single lookup point so every
// auto-emit hook agrees on where crumbs live.
func crumbsFromContext(ctx context.Context) *breadcrumbBuffer {
	if ctx == nil {
		return nil
	}
	if s := stateFromContext(ctx); s != nil {
		s.crumbsOnce.Do(func() {
			if s.crumbs == nil {
				s.crumbs = newBreadcrumbBuffer(defaultMaxBreadcrumbs)
			}
		})
		return s.crumbs
	}
	return nil
}

// WithBreadcrumbs ensures a request-scoped breadcrumb buffer exists on the
// context, returning a context that carries it. The inbound middleware already
// installs a state bag (which lazily grows a buffer on first crumb), so callers
// using the middleware never need this. It exists for library consumers who
// want an auto-breadcrumb trail on a context they manage themselves, e.g. a
// background job:
//
//	ctx = allstak.WithBreadcrumbs(ctx)
//	client.AddBreadcrumb(ctx, allstak.Breadcrumb{Category: "job", Message: "started"})
//	// ... work that may call client.CaptureException(ctx, err) ...
//
// If a state bag is already present the same context is returned (the bag is a
// shared pointer), so this is safe to call redundantly.
func WithBreadcrumbs(ctx context.Context) context.Context {
	if stateFromContext(ctx) != nil {
		// A bag already exists; crumbsFromContext will lazily attach a buffer.
		return ctx
	}
	s := newRequestState()
	s.crumbs = newBreadcrumbBuffer(defaultMaxBreadcrumbs)
	return withRequestState(ctx, s)
}

// AddBreadcrumb records a single breadcrumb on the request-scoped trail. The
// crumb is buffered (never sent on its own); it is attached to the next error
// captured on this context via enrichFromContext, so the dashboard shows the
// trail of activity leading up to a failure.
//
// A blank Timestamp is stamped with the current UTC time and a blank Level
// defaults to "info" so callers can pass a minimal crumb. The call is a no-op
// (fail-open) when the client is closed or no request-scoped state bag is on
// the context — the SDK never errors a caller for an un-instrumented context.
//
// This is the manual escape hatch; the http/db/log layers emit crumbs
// automatically when they run inside an instrumented request.
func (c *Client) AddBreadcrumb(ctx context.Context, bc Breadcrumb) {
	if c == nil || c.closed.Load() {
		return
	}
	addBreadcrumb(ctx, bc)
}

// addBreadcrumb is the internal, client-free crumb writer used by the
// auto-emit hooks (which already hold a *Client but want a single normalizing
// path). It stamps defaults and appends to the request-scoped buffer.
func addBreadcrumb(ctx context.Context, bc Breadcrumb) {
	buf := crumbsFromContext(ctx)
	if buf == nil {
		return
	}
	if bc.Timestamp == "" {
		bc.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if bc.Level == "" {
		bc.Level = "info"
	}
	buf.add(bc)
}

// ── Shared crumb constructors ───────────────────────────────────────────────
//
// These keep the auto-emit hooks (inbound middleware, outbound RoundTripper,
// GORM after-callback, log helpers) consistent: same Type/Category vocabulary,
// same Data keys, so the dashboard renders one coherent timeline regardless of
// which layer produced the crumb.

// httpBreadcrumb builds an "http" breadcrumb for an inbound or outbound request.
// A status >= 400 (or 0, a transport failure) escalates the level so the trail
// visually flags the failing hop. direction is "inbound" or "outbound".
func httpBreadcrumb(direction, method, host, path string, status, durationMs int) Breadcrumb {
	level := "info"
	if status == 0 || status >= 400 {
		level = "warning"
	}
	return Breadcrumb{
		Type:     "http",
		Category: "http." + direction,
		Message:  method + " " + host + path,
		Level:    level,
		Data: map[string]any{
			"direction":  direction,
			"method":     method,
			"host":       host,
			"path":       path,
			"statusCode": status,
			"durationMs": durationMs,
		},
	}
}

// dbBreadcrumb builds a "query" breadcrumb for a database statement. A failed
// query escalates the level. The normalized (value-stripped) SQL is used as the
// message so no bound values leak into the trail.
func dbBreadcrumb(queryType, normalizedSQL, status string, durationMs int64, rows int) Breadcrumb {
	level := "info"
	if status == "error" {
		level = "error"
	}
	msg := normalizedSQL
	if msg == "" {
		msg = queryType
	}
	return Breadcrumb{
		Type:     "query",
		Category: "db.query",
		Message:  msg,
		Level:    level,
		Data: map[string]any{
			"queryType":    queryType,
			"status":       status,
			"durationMs":   durationMs,
			"rowsAffected": rows,
		},
	}
}

// AddHTTPBreadcrumb records an http breadcrumb on the request-scoped trail.
// It is the integration-facing entry point so framework adapters living in
// their own modules (which cannot reach the unexported helpers) can emit the
// same crumb shape the core net/http middleware does. No-op when no
// request-state bag is on the context. direction is "inbound" or "outbound".
func AddHTTPBreadcrumb(ctx context.Context, direction, method, host, path string, statusCode, durationMs int) {
	addBreadcrumb(ctx, httpBreadcrumb(direction, method, host, path, statusCode, durationMs))
}

// AddDBBreadcrumb records a database-query breadcrumb on the request-scoped
// trail. It is the integration-facing entry point for DB instrumentation
// modules (e.g. the GORM plugin). The SQL should already be NORMALIZED so no
// bound values reach the trail. No-op when no request-state bag is on the
// context.
func AddDBBreadcrumb(ctx context.Context, queryType, normalizedSQL, status string, durationMs int64, rowsAffected int) {
	addBreadcrumb(ctx, dbBreadcrumb(queryType, normalizedSQL, status, durationMs, rowsAffected))
}

// logBreadcrumb builds a "log" breadcrumb mirroring a structured log line so
// the trail interleaves logs with http/db activity in chronological order.
func logBreadcrumb(level, message string, fields []Field) Breadcrumb {
	var data map[string]any
	if len(fields) > 0 {
		data = make(map[string]any, len(fields))
		for _, f := range fields {
			data[f.Key] = f.Value
		}
	}
	return Breadcrumb{
		Type:     "log",
		Category: "log",
		Message:  message,
		Level:    level,
		Data:     data,
	}
}
