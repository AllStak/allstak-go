// Package allstakcron wraps a robfig/cron job so each run pings AllStak's
// cron monitor endpoint, producing a heartbeat that populates the Cron
// Monitors section of the dashboard.
//
// Typical usage:
//
//	c := cron.New()
//	c.AddFunc("@every 1m", allstakcron.Wrap(client, "heartbeat-slug", func(ctx context.Context) error {
//	    return doWork(ctx)
//	}))
//	c.Start()
//
// The wrapper sends a "success" heartbeat on nil error, and a "failed"
// heartbeat (plus an error capture with stack trace) when the job panics
// or returns a non-nil error.
//
// For users who do not run their own cron daemon, the package also
// exposes RunJob which is a one-shot wrapper suitable for background
// goroutines kicked off by a timer or a queue consumer.
package allstakcron

import (
	"context"
	"time"

	allstak "github.com/AllStak/allstak-go"
)

// JobFunc is the customer-supplied work function. It receives a context
// that will already carry the job's trace/span IDs, and should return an
// error when the job should be recorded as failed.
type JobFunc func(ctx context.Context) error

// Wrap adapts a JobFunc into a func() suitable for passing to
// cron.Cron.AddFunc. The returned function runs the job, measures
// duration, captures panics, and sends a heartbeat to AllStak.
func Wrap(client *allstak.Client, slug string, job JobFunc) func() {
	return func() {
		_ = RunJob(context.Background(), client, slug, job)
	}
}

// RunJob is the plain-context variant of Wrap, suitable for direct
// invocation from anywhere in user code. It returns the error the job
// returned (or nil) so the caller can still react to it.
//
// A fresh span is started for each run so every execution shows up as
// its own trace in the dashboard. The span ID is propagated through the
// ctx passed to job, so any outbound HTTP or DB work done inside the
// job is correctly linked to the heartbeat.
func RunJob(ctx context.Context, client *allstak.Client, slug string, job JobFunc) error {
	if client == nil || job == nil {
		return nil
	}

	start := time.Now()
	traceID := allstak.NewTraceID()
	spanID := allstak.NewSpanID()
	ctx = allstak.WithContextSpan(ctx, traceID, spanID, "")

	// Capture panics inside the job and convert them to fatal errors +
	// a failed heartbeat. We do not re-panic — a crashed cron job
	// should never take down the scheduler.
	var jobErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				client.CapturePanicValue(ctx, r)
				jobErr = &panicError{value: r}
			}
		}()
		jobErr = job(ctx)
	}()

	durationMs := int(time.Since(start).Milliseconds())
	status := "success"
	message := ""
	if jobErr != nil {
		status = "failed"
		message = jobErr.Error()
		client.CaptureException(ctx, jobErr)
	}

	// Fire-and-forget the heartbeat with a bounded context so a hung
	// backend can't stall the next scheduled run.
	hbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = client.SendHeartbeat(hbCtx, allstak.HeartbeatPayload{
		Slug:       slug,
		Status:     status,
		DurationMs: durationMs,
		Message:    message,
	})

	// Record the whole run as a top-level span so the Traces page shows
	// a single row per invocation.
	client.CaptureSpan(allstak.SpanItem{
		TraceID:         traceID,
		SpanID:          spanID,
		Operation:       "cron." + slug,
		Status:          status,
		DurationMs:      time.Since(start).Milliseconds(),
		StartTimeMillis: start.UnixMilli(),
		EndTimeMillis:   time.Now().UnixMilli(),
	})

	return jobErr
}

// panicError wraps a recovered panic value so we can return it from
// RunJob as a normal error. The Error() method mirrors what fmt would
// produce for an unwrapped panic.
type panicError struct {
	value any
}

func (e *panicError) Error() string {
	switch v := e.value.(type) {
	case error:
		return "panic: " + v.Error()
	case string:
		return "panic: " + v
	default:
		return "panic in cron job"
	}
}
