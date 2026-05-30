// Package allstakzap bridges Uber's zap logger into AllStak. Wrap your zap
// logger (or build a standalone core) and every log entry ships a structured
// log to the AllStak Logs stream; ERROR-level (and above) entries that carry an
// error field are additionally promoted to the Errors stream as first-class,
// grouped errors.
//
// Zero-config, default-on after wiring the core once:
//
//	base, _ := zap.NewProduction()
//	logger := allstakzap.Wrap(base, client, nil)
//	logger.Error("charge failed", zap.Error(err), zap.String("orderId", id))
//
// Wrap tees: entries continue to your existing zap core AND ship to AllStak.
// Use NewCore to build a standalone AllStak-only core.
//
// Trace / request / user correlation: zap has no context plumbing of its own,
// so attach the active context per call site with WithContext, which carries
// the context to the AllStak core so it can stamp trace / span / request / user
// ids on the shipped log.
package allstakzap

import (
	"context"
	allstak "github.com/AllStak/allstak-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Options configures the zap bridge.
type Options struct {
	// MinLevel is the lowest level shipped to AllStak. Defaults to InfoLevel.
	MinLevel zapcore.Level
}

// Core is a zapcore.Core that ships entries to AllStak. It can tee with another
// core via zapcore.NewTee (what Wrap does) or stand alone (NewCore).
type Core struct {
	client   *allstak.Client
	minLevel zapcore.Level
	fields   []zapcore.Field
	ctx      context.Context
}

// NewCore builds a standalone AllStak zapcore.Core. Most users want Wrap, which
// tees this with their existing core so local logging keeps working.
func NewCore(client *allstak.Client, opts *Options) *Core {
	min := zapcore.InfoLevel
	if opts != nil {
		min = opts.MinLevel
	}
	return &Core{client: client, minLevel: min, ctx: context.Background()}
}

// Wrap returns a *zap.Logger that tees the given logger's core with an AllStak
// core, so entries ship to AllStak AND continue to the original destinations.
func Wrap(logger *zap.Logger, client *allstak.Client, opts *Options) *zap.Logger {
	return logger.WithOptions(zap.WrapCore(func(existing zapcore.Core) zapcore.Core {
		return zapcore.NewTee(existing, NewCore(client, opts))
	}))
}

// Enabled implements zapcore.Core.
func (c *Core) Enabled(l zapcore.Level) bool { return l >= c.minLevel }

// With implements zapcore.Core — returns a child core carrying the added
// fields. If a context field is present (see WithContext) it is lifted onto the
// core so Write can stamp trace/request ids.
func (c *Core) With(fields []zapcore.Field) zapcore.Core {
	clone := c.clone()
	clone.fields = append(append([]zapcore.Field{}, c.fields...), fields...)
	clone.ctx = contextFromFields(c.ctx, fields)
	return clone
}

// Check implements zapcore.Core.
func (c *Core) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

// Write implements zapcore.Core — the hot path. It encodes the entry's fields
// into a plain map, extracts any error/context, and hands the record to the
// shared SDK bridge (which ships the log and promotes error+ with an error).
func (c *Core) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	all := append(append([]zapcore.Field{}, c.fields...), fields...)
	ctx := contextFromFields(c.ctx, fields)

	data, err := fieldsToMap(all)
	c.client.BridgeLog(ctx, allstak.BridgeRecord{
		Level:   ent.Level.String(),
		Message: ent.Message,
		Fields:  data,
		Err:     err,
	})
	return nil
}

// Sync implements zapcore.Core. The SDK flushes on its own schedule and on
// Close, so there is nothing to force here.
func (c *Core) Sync() error { return nil }

func (c *Core) clone() *Core {
	return &Core{client: c.client, minLevel: c.minLevel, fields: c.fields, ctx: c.ctx}
}

// fieldsToMap renders zap fields into a flat map[string]any and returns the
// first error value it encounters (for error-stream promotion). It uses zap's
// own MapObjectEncoder so every field type encodes exactly as zap would render
// it elsewhere.
func fieldsToMap(fields []zapcore.Field) (map[string]any, error) {
	enc := zapcore.NewMapObjectEncoder()
	var firstErr error
	for _, f := range fields {
		// Skip the smuggled context field — it is plumbing, not log data.
		if f.Key == ctxFieldKey {
			continue
		}
		if f.Type == zapcore.ErrorType {
			if e, ok := f.Interface.(error); ok && firstErr == nil {
				firstErr = e
			}
		}
		f.AddTo(enc)
	}
	return enc.Fields, firstErr
}

// ctxFieldKey is the field key WithContext uses to smuggle a context.Context
// through zap's field list so Write can stamp trace/request ids.
const ctxFieldKey = "allstak.ctx"

// WithContext returns a zap.Field that carries the active context so the
// AllStak core can stamp trace / span / request / user ids on the shipped log.
// Pass it alongside your other fields:
//
//	logger.Error("charge failed", allstakzap.WithContext(ctx), zap.Error(err))
func WithContext(ctx context.Context) zapcore.Field {
	return zap.Any(ctxFieldKey, ctx)
}

// contextFromFields scans fields for a context carried by WithContext, falling
// back to the provided default. The context field is consumed (the bridge does
// not re-emit it as a log attribute).
func contextFromFields(def context.Context, fields []zapcore.Field) context.Context {
	for _, f := range fields {
		if f.Key == ctxFieldKey {
			if ctx, ok := f.Interface.(context.Context); ok && ctx != nil {
				return ctx
			}
		}
	}
	if def == nil {
		return context.Background()
	}
	return def
}
