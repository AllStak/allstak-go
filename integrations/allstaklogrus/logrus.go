// Package allstaklogrus bridges sirupsen/logrus into AllStak. Register the hook
// once and every logrus entry ships a structured log to the AllStak Logs
// stream; ERROR/FATAL/PANIC entries that carry an error field are additionally
// promoted to the Errors stream as first-class, grouped errors.
//
// Zero-config, default-on after registering the hook:
//
//	logrus.AddHook(allstaklogrus.NewHook(client, nil))
//	logrus.WithContext(ctx).WithError(err).Error("charge failed")
//
// logrus keeps writing to its own output (stderr by default), so this hook is
// purely additive — it tees to AllStak. Trace / request / user correlation is
// picked up from the entry's Context (logrus.WithContext) when present.
package allstaklogrus

import (
	"context"
	allstak "github.com/AllStak/allstak-go"
	"github.com/sirupsen/logrus"
)

// Options configures the logrus hook.
type Options struct {
	// Levels restricts which levels fire the hook. When nil, all levels fire
	// (AllStak applies its own MinLevel semantics via the log stream).
	Levels []logrus.Level
}

// Hook implements logrus.Hook, shipping fired entries to AllStak.
type Hook struct {
	client *allstak.Client
	levels []logrus.Level
}

// NewHook builds a logrus hook wired to the given AllStak client. A nil opts
// fires on all levels.
func NewHook(client *allstak.Client, opts *Options) *Hook {
	levels := logrus.AllLevels
	if opts != nil && opts.Levels != nil {
		levels = opts.Levels
	}
	return &Hook{client: client, levels: levels}
}

// Levels implements logrus.Hook.
func (h *Hook) Levels() []logrus.Level { return h.levels }

// Fire implements logrus.Hook. It flattens the entry's fields, extracts the
// error (logrus.ErrorKey or any error-typed value), resolves the context, and
// hands the record to the shared SDK bridge.
func (h *Hook) Fire(e *logrus.Entry) error {
	ctx := e.Context
	if ctx == nil {
		ctx = context.Background()
	}

	fields := make(map[string]any, len(e.Data))
	var firstErr error
	for k, v := range e.Data {
		if err, ok := v.(error); ok {
			if firstErr == nil {
				firstErr = err
			}
			fields[k] = err.Error()
			continue
		}
		fields[k] = v
	}

	h.client.BridgeLog(ctx, allstak.BridgeRecord{
		Level:   e.Level.String(),
		Message: e.Message,
		Fields:  fields,
		Err:     firstErr,
	})
	return nil
}
