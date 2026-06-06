package allstak

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"
)

// CaptureException is the high-level error-capture helper. It unwraps the
// error chain, extracts a stack trace, and enqueues an ErrorPayload with
// level="error". Context-bound metadata (user, request, trace) is pulled
// from the context using the helpers in context.go.
//
// This is the method customers should reach for first. The lower-level
// Client.CaptureError is still exported for integrations that need to
// populate the payload themselves.
func (c *Client) CaptureException(ctx context.Context, err error) {
	if err == nil || c.closed.Load() {
		return
	}

	p := ErrorPayload{
		ExceptionClass: exceptionClassOf(err),
		Message:        unwrapMessage(err),
		StackTrace:     captureStack(1),
		Frames:         captureStructuredFrames(1),
		Level:          "error",
		Mechanism:      "captureException",
		Handled:        handledPtr(true),
	}
	c.enrichFromContext(ctx, &p)
	c.CaptureError(p)
}

// CaptureExceptionWithLevel is identical to CaptureException but lets the
// caller pick a level (debug/info/warn/error/fatal). Useful for non-fatal
// warnings captured via the same API surface.
func (c *Client) CaptureExceptionWithLevel(ctx context.Context, err error, level string) {
	if err == nil || c.closed.Load() {
		return
	}
	p := ErrorPayload{
		ExceptionClass: exceptionClassOf(err),
		Message:        unwrapMessage(err),
		StackTrace:     captureStack(1),
		Frames:         captureStructuredFrames(1),
		Level:          level,
		Mechanism:      "captureException",
		Handled:        handledPtr(true),
	}
	c.enrichFromContext(ctx, &p)
	c.CaptureError(p)
}

// CaptureMessage enqueues a plain-text event with no stack trace. Useful
// for notable business events that aren't really errors. Level defaults
// to "info" if empty.
func (c *Client) CaptureMessage(ctx context.Context, level, message string) {
	if c.closed.Load() || message == "" {
		return
	}
	if level == "" {
		level = "info"
	}
	p := ErrorPayload{
		ExceptionClass: "Message",
		Message:        message,
		Level:          level,
		Mechanism:      "captureMessage",
		Handled:        handledPtr(true),
	}
	c.enrichFromContext(ctx, &p)
	c.CaptureError(p)
}

// unwrapMessage walks the error chain and returns the outermost message.
// If the chain is empty, returns err.Error() directly. This matches the
// AllStak convention of showing the user-visible message at the top.
func unwrapMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Recover is designed to be used in a deferred call to catch panics,
// convert them to an error, and forward to CaptureException. It re-panics
// so the host application's own recovery logic still runs.
//
//	defer client.Recover(ctx)
//
// If you want to swallow the panic (e.g. in a goroutine that must not
// take down the process) use RecoverAndSuppress instead.
func (c *Client) Recover(ctx context.Context) {
	if r := recover(); r != nil {
		c.capturePanic(ctx, r)
		panic(r) // re-panic so host recovery still runs
	}
}

// RecoverAndSuppress captures a panic and does NOT re-panic. Use this in
// fire-and-forget goroutines where a crash would take down the process.
func (c *Client) RecoverAndSuppress(ctx context.Context) {
	if r := recover(); r != nil {
		c.capturePanic(ctx, r)
	}
}

// CapturePanicValue is the integration-facing wrapper around the internal
// panic-capture logic. Framework middlewares that do their own recover()
// call this with the recovered value to record it as a fatal error.
// It is a no-op if r is nil or the client is closed.
func (c *Client) CapturePanicValue(ctx context.Context, r any) {
	if r == nil || c.closed.Load() {
		return
	}
	c.capturePanic(ctx, r)
}

// capturePanic synthesizes an ErrorPayload from a recovered panic value.
// The payload gets the raw debug.Stack so users see the exact moment of
// the panic, not the Recover call site.
func (c *Client) capturePanic(ctx context.Context, r any) {
	var err error
	switch v := r.(type) {
	case error:
		err = v
	case string:
		err = errors.New(v)
	default:
		err = fmt.Errorf("panic: %v", v)
	}

	// debug.Stack() gives a multi-line human-readable dump; split into
	// lines so it renders nicely in the dashboard's stack frame view.
	stack := splitLines(string(debug.Stack()))

	p := ErrorPayload{
		ExceptionClass: "runtime.Panic: " + exceptionClassOf(err),
		Message:        err.Error(),
		StackTrace:     stack,
		Level:          "fatal",
		Mechanism:      "panic",
		Handled:        handledPtr(false),
	}
	c.enrichFromContext(ctx, &p)
	c.CaptureError(p)
}

// splitLines splits a newline-separated stack dump into a slice, trimming
// any trailing empty line from debug.Stack().
func splitLines(s string) []string {
	lines := make([]string, 0, 32)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				lines = append(lines, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func handledPtr(v bool) *bool {
	return &v
}

// ── Log helpers ───────────────────────────────────────────────────────────

// Info enqueues an info-level log event. This is the cheapest way to ship
// a structured message through the SDK without touching the low-level
// CaptureLog surface.
func (c *Client) Info(ctx context.Context, message string, fields ...Field) {
	c.log(ctx, "info", message, fields)
}

// Warn enqueues a warn-level log event.
func (c *Client) Warn(ctx context.Context, message string, fields ...Field) {
	c.log(ctx, "warn", message, fields)
}

// Error enqueues an error-level log event. Note this does not create an
// Error in the Errors dashboard — use CaptureException for that.
func (c *Client) Error(ctx context.Context, message string, fields ...Field) {
	c.log(ctx, "error", message, fields)
}

// Debug enqueues a debug-level log event.
func (c *Client) Debug(ctx context.Context, message string, fields ...Field) {
	c.log(ctx, "debug", message, fields)
}

// Field is a key/value pair for structured logging. Kept deliberately
// simple — we don't want users to reach for the allstak logger as a
// replacement for zap/slog, just as a thin bridge into the dashboard.
type Field struct {
	Key   string
	Value any
}

// F is a convenience constructor for Field.
func F(key string, value any) Field {
	return Field{Key: key, Value: value}
}

func (c *Client) log(ctx context.Context, level, message string, fields []Field) {
	if c.closed.Load() || message == "" {
		return
	}
	var meta map[string]any
	if len(fields) > 0 {
		meta = make(map[string]any, len(fields))
		for _, f := range fields {
			meta[f.Key] = f.Value
		}
	}
	p := LogPayload{
		Level:    level,
		Message:  message,
		Metadata: meta,
	}
	if tid, sid := TraceFromContext(ctx); tid != "" {
		p.TraceID = tid
		p.SpanID = sid
	}
	if u := UserFromContext(ctx); u != nil {
		p.UserID = u.ID
	}
	if rid := RequestIDFromContext(ctx); rid != "" {
		p.RequestID = rid
	}
	// Auto-breadcrumb: mirror the log line onto the request-scoped trail so a
	// later error capture shows the structured logs that preceded it,
	// interleaved chronologically with http/db crumbs.
	addBreadcrumb(ctx, logBreadcrumb(level, message, fields))
	c.CaptureLog(p)
}

// ── Span helpers ──────────────────────────────────────────────────────────

// StartSpan creates a new span whose lifetime is bounded by the returned
// Finish call. The span inherits its trace ID from any parent span on the
// context; otherwise a fresh trace is started.
//
// Usage:
//
//	ctx, finish := client.StartSpan(ctx, "checkout.charge")
//	defer finish(err)
func (c *Client) StartSpan(ctx context.Context, operation string) (context.Context, func(err error)) {
	start := time.Now()
	parent := SpanFromContext(ctx)

	traceID := ""
	parentSpanID := ""
	// Sampling decision: a child span inherits its parent trace's sampled
	// decision (W3C: the head-of-trace decision is sticky). A root span draws
	// from TracesSampleRate. This keeps a single trace internally consistent.
	sampled := true
	if parent != nil {
		traceID = parent.TraceID
		parentSpanID = parent.SpanID
		sampled = parent.Sampled
	} else {
		sampled = shouldSampleTrace(c.cfg.TracesSampleRate)
	}
	if traceID == "" {
		traceID = NewTraceID()
	}
	spanID := NewSpanID()

	ctx = withSpan(ctx, &SpanContext{TraceID: traceID, SpanID: spanID, ParentSpanID: parentSpanID, Sampled: sampled})

	return ctx, func(err error) {
		if c.closed.Load() {
			return
		}
		// Not sampled — propagate trace context (already on ctx) but do not
		// record the span itself.
		if !sampled {
			return
		}
		end := time.Now()
		status := "ok"
		if err != nil {
			status = "error"
		}
		c.CaptureSpan(SpanItem{
			TraceID:         traceID,
			SpanID:          spanID,
			ParentSpanID:    parentSpanID,
			Operation:       operation,
			Status:          status,
			DurationMs:      end.Sub(start).Milliseconds(),
			StartTimeMillis: start.UnixMilli(),
			EndTimeMillis:   end.UnixMilli(),
		})
	}
}
