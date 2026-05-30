// Package allstakslog bridges the standard library's log/slog into AllStak.
// Attach the handler once and every slog call ships a structured log to the
// AllStak Logs stream — with the request's trace / span / request / user ids
// stamped automatically from the context passed to the logger. ERROR-level
// records that carry an `error` attribute are additionally promoted to the
// Errors stream so a logged failure surfaces as a first-class, grouped error.
//
// Zero-config, default-on after one line of wiring:
//
//	logger := slog.New(allstakslog.NewHandler(client, nil))
//	slog.SetDefault(logger)
//	// ...
//	slog.ErrorContext(ctx, "charge failed", "err", err, "orderId", id)
//
// The handler is a TEE by default: it forwards to an inner handler (a text or
// JSON handler writing to stderr) so local logs keep working, AND ships to
// AllStak. Pass Options.Inner = nil-disabling via Options to ship only.
//
// This package depends only on the standard library, so importing it adds no
// third-party modules to your build.
package allstakslog

import (
	"context"
	"log/slog"
	"os"

	allstak "github.com/AllStak/allstak-go"
)

// Options configures the slog bridge. The zero value is valid: it tees to a
// stderr text handler at LevelInfo and ships everything to AllStak.
type Options struct {
	// Inner is the handler records are forwarded to in addition to AllStak so
	// local logging keeps working (a tee). When nil, NewHandler installs a
	// text handler writing to stderr. Set DisableInner to ship to AllStak only.
	Inner slog.Handler

	// DisableInner ships records to AllStak ONLY, with no local forwarding.
	DisableInner bool

	// MinLevel is the lowest level shipped to AllStak. Records below it are
	// still forwarded to Inner (subject to Inner's own level). Defaults to
	// slog.LevelInfo so debug noise stays local unless explicitly enabled.
	MinLevel slog.Level
}

// preResolved is an attr already flattened to a (possibly group-prefixed) key
// with its error value (if any) extracted. WithAttrs bakes attrs into this form
// at the time they are added so a later WithGroup does not retroactively prefix
// them — matching slog's group-nesting semantics.
type preResolved struct {
	key string
	val any
	err error
}

// Handler is an slog.Handler that ships records to AllStak (and, by default,
// tees them to an inner handler). It is safe for concurrent use.
type Handler struct {
	client   *allstak.Client
	inner    slog.Handler
	minLevel slog.Level
	// attrs are pre-resolved (group-prefixed at add time) accumulated attrs
	// from WithAttrs calls.
	attrs []preResolved
	// groups is the CURRENTLY open group path applied to record attrs.
	groups []string
}

// NewHandler builds an slog.Handler wired to the given AllStak client. A nil
// opts uses the defaults (tee to a stderr text handler, ship LevelInfo+).
func NewHandler(client *allstak.Client, opts *Options) *Handler {
	o := Options{}
	if opts != nil {
		o = *opts
	}
	inner := o.Inner
	if inner == nil && !o.DisableInner {
		inner = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: o.MinLevel})
	}
	if o.DisableInner {
		inner = nil
	}
	return &Handler{
		client:   client,
		inner:    inner,
		minLevel: o.MinLevel,
	}
}

// Enabled reports whether a record at the given level should be processed. We
// process when either AllStak wants it (>= minLevel) or the inner handler does,
// so the tee never silently drops a record one side would have kept.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= h.minLevel {
		return true
	}
	return h.inner != nil && h.inner.Enabled(ctx, level)
}

// Handle ships the record to AllStak (when at/above MinLevel) and forwards it
// to the inner handler. The context carries the AllStak trace/request ids, so
// records logged via slog.*Context correlate to the active request.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= h.minLevel {
		h.shipToAllStak(ctx, r)
	}
	if h.inner != nil && h.inner.Enabled(ctx, r.Level) {
		return h.inner.Handle(ctx, r)
	}
	return nil
}

func (h *Handler) shipToAllStak(ctx context.Context, r slog.Record) {
	fields := make(map[string]any, r.NumAttrs()+len(h.attrs))
	var err error
	// Pre-resolved attrs (from WithAttrs) carry their own keys/errors already.
	for _, pr := range h.attrs {
		fields[pr.key] = pr.val
		if pr.err != nil && err == nil {
			err = pr.err
		}
	}
	// Record attrs get the CURRENTLY open group prefix.
	r.Attrs(func(a slog.Attr) bool {
		if pr, ok := resolveAttr(h.groups, a); ok {
			fields[pr.key] = pr.val
			if pr.err != nil && err == nil {
				err = pr.err
			}
		}
		return true
	})
	h.client.BridgeLog(ctx, allstak.BridgeRecord{
		Level:   levelString(r.Level),
		Message: r.Message,
		Fields:  fields,
		Err:     err,
	})
}

// resolveAttr flattens a slog.Attr into a preResolved with any active group
// prefix baked into the key, extracting the first error-typed value. Returns
// false for the empty attr.
func resolveAttr(groups []string, a slog.Attr) (preResolved, bool) {
	if a.Equal(slog.Attr{}) {
		return preResolved{}, false
	}
	key := a.Key
	if len(groups) > 0 {
		key = joinGroups(groups) + "." + key
	}
	v := a.Value.Resolve()
	if v.Kind() == slog.KindAny {
		if e, ok := v.Any().(error); ok {
			return preResolved{key: key, val: e.Error(), err: e}, true
		}
	}
	return preResolved{key: key, val: v.Any()}, true
}

func joinGroups(groups []string) string {
	out := groups[0]
	for _, g := range groups[1:] {
		out += "." + g
	}
	return out
}

// WithAttrs returns a handler that includes the given attrs on every record.
// The attrs are resolved with the CURRENT group path baked in, so a subsequent
// WithGroup does not retroactively re-prefix them (slog semantics).
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := h.clone()
	resolved := make([]preResolved, 0, len(h.attrs)+len(attrs))
	resolved = append(resolved, h.attrs...)
	for _, a := range attrs {
		if pr, ok := resolveAttr(h.groups, a); ok {
			resolved = append(resolved, pr)
		}
	}
	nh.attrs = resolved
	if h.inner != nil {
		nh.inner = h.inner.WithAttrs(attrs)
	}
	return nh
}

// WithGroup returns a handler that nests subsequent attrs under name.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := h.clone()
	nh.groups = append(append([]string{}, h.groups...), name)
	if h.inner != nil {
		nh.inner = h.inner.WithGroup(name)
	}
	return nh
}

func (h *Handler) clone() *Handler {
	return &Handler{
		client:   h.client,
		inner:    h.inner,
		minLevel: h.minLevel,
		attrs:    h.attrs,
		groups:   h.groups,
	}
}

// levelString maps an slog.Level to the SDK's level vocabulary.
func levelString(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}
