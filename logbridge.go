package allstak

import (
	"context"
	"strings"
)

// BridgeRecord is the normalized shape a logging-framework bridge (slog, zap,
// logrus) hands to the SDK. It decouples the bridges — which live in their own
// modules — from the internal LogPayload/ErrorPayload wire types, so the
// promotion rule (ERROR+ with an attached error becomes an Errors event) lives
// in ONE place and every bridge behaves identically.
type BridgeRecord struct {
	// Level is the normalized level string: "debug"/"info"/"warn"/"error".
	Level string
	// Message is the log message.
	Message string
	// Fields are the structured attributes from the log record. They become
	// the log event's metadata and, when promoted, the error's metadata.
	Fields map[string]any
	// Err is the error value attached to the record, if any. When Level is
	// error (or fatal) AND Err is non-nil, the record is ALSO promoted to the
	// Errors stream so it surfaces as a first-class error with a stack-derived
	// fingerprint — not just a log line.
	Err error
}

// BridgeLog ships a logging-framework record through the SDK. It ALWAYS emits a
// structured log to /ingest/v1/logs (stamped with the context's trace / span /
// request / user ids), and ADDITIONALLY promotes the record to the Errors
// stream when it is error/fatal level and carries an error value. This is the
// single entry point the slog / zap / logrus bridges call so promotion and
// id-stamping behave identically across all three.
//
// It is a no-op when the client is closed or the message is empty.
func (c *Client) BridgeLog(ctx context.Context, rec BridgeRecord) {
	if c == nil || c.closed.Load() || rec.Message == "" {
		return
	}
	level := normalizeBridgeLevel(rec.Level)

	// 1. Always ship the structured log.
	p := LogPayload{
		Level:    level,
		Message:  rec.Message,
		Metadata: rec.Fields,
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
	c.CaptureLog(p)

	// Mirror the log line onto the breadcrumb trail too, so a later capture
	// shows framework logs interleaved with http/db crumbs.
	addBreadcrumb(ctx, Breadcrumb{
		Type:     "log",
		Category: "log",
		Message:  rec.Message,
		Level:    level,
		Data:     rec.Fields,
	})

	// 2. Promote error+ with an attached error to the Errors stream so it is a
	// first-class, grouped error — not just a searchable log line.
	if rec.Err != nil && bridgeLevelIsError(level) {
		c.CaptureException(ctx, rec.Err)
	}
}

// normalizeBridgeLevel maps the many framework level spellings onto the four
// wire levels the SDK uses. Unknown values fall back to "info".
func normalizeBridgeLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "trace", "debug":
		return "debug"
	case "info", "notice", "":
		return "info"
	case "warn", "warning":
		return "warn"
	case "error", "err", "fatal", "panic", "dpanic", "critical", "crit":
		return "error"
	default:
		return "info"
	}
}

// bridgeLevelIsError reports whether a normalized level warrants error-stream
// promotion (error and above).
func bridgeLevelIsError(level string) bool {
	return level == "error"
}
