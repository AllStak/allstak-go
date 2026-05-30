package allstak

import (
	"context"
	"net/http"
)

// SafeGo launches fn in a new goroutine with a deferred AllStak panic guard
// installed. If fn panics, the panic is captured as a FATAL error (with the
// exact panic stack), then RE-PANICKED so the runtime's default behavior is
// preserved — an uncaught goroutine panic still crashes the process exactly as
// it would without the SDK. This is the safe default: the SDK observes the
// crash for release-health and the errors stream without altering control flow.
//
//	client.SafeGo(ctx, func() {
//	    doBackgroundWork() // a panic here is captured AND still crashes the process
//	})
//
// Wrap each goroutine in a worker pool or errgroup so background panics — which
// the inbound HTTP middleware can never see — are still reported:
//
//	for i := 0; i < workers; i++ {
//	    client.SafeGo(ctx, func() { for job := range jobs { handle(job) } })
//	}
//
// ctx may be nil. When non-nil its user/request/trace context (and any
// breadcrumb trail) enriches the captured panic, so a goroutine spawned from a
// request carries that request's correlation.
//
// To capture WITHOUT crashing the process (a fire-and-forget goroutine that
// must not take down the program), use SafeGoSuppress.
func (c *Client) SafeGo(ctx context.Context, fn func()) {
	if fn == nil {
		return
	}
	go func() {
		defer c.recoverAndRepanic(ctx)
		fn()
	}()
}

// SafeGoSuppress is SafeGo that does NOT re-panic: a panic in fn is captured as
// a fatal error and then swallowed, so the goroutine exits cleanly and the
// process keeps running. Use this for truly fire-and-forget background work
// where a single failed task must not crash the whole service.
//
//	client.SafeGoSuppress(ctx, func() {
//	    flushMetrics() // a panic here is captured; the process survives
//	})
func (c *Client) SafeGoSuppress(ctx context.Context, fn func()) {
	if fn == nil {
		return
	}
	go func() {
		defer c.RecoverAndSuppress(ctx)
		fn()
	}()
}

// recoverAndRepanic is the deferred guard SafeGo installs. It mirrors
// Client.Recover but is kept separate so the captured stack frame skipping is
// predictable from the goroutine entry point.
func (c *Client) recoverAndRepanic(ctx context.Context) {
	if r := recover(); r != nil {
		c.capturePanic(ctx, r)
		panic(r) // re-panic so the runtime's default crash behavior is preserved
	}
}

// RecoverHandler wraps an http.Handler with a deferred panic guard. A panic in
// the wrapped handler is captured as a fatal error and a 500 is written if the
// handler had not yet committed a response; the panic is NOT re-thrown so a
// single bad request cannot crash the server. It is a lighter alternative to
// the full Middleware when you only want crash safety (no request/trace
// capture) — for full instrumentation prefer Middleware, which already recovers.
//
//	mux.Handle("/admin", client.RecoverHandler(adminHandler))
func (c *Client) RecoverHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if rec := recover(); rec != nil {
				c.capturePanic(ctx, rec)
				if !rw.wroteHeader {
					http.Error(rw, "internal server error", http.StatusInternalServerError)
				}
			}
		}()
		next.ServeHTTP(rw, r)
	})
}

// RecoverHandlerFunc is RecoverHandler for the http.HandlerFunc shape, so it
// composes cleanly with mux.HandleFunc.
//
//	mux.HandleFunc("/admin", client.RecoverHandlerFunc(adminFunc))
func (c *Client) RecoverHandlerFunc(next http.HandlerFunc) http.HandlerFunc {
	h := c.RecoverHandler(next)
	return h.ServeHTTP
}

// ── Package-level convenience over the default client ───────────────────────

// Go is the package-level goroutine guard over the default client installed by
// InitFromEnv (or SetDefault). It is the zero-config form of SafeGo: a panic in
// fn is captured and re-panicked. If no default client has been installed, the
// goroutine still runs with a guard that re-panics — the panic is simply not
// reported (the SDK is a no-op rather than an error). This keeps allstak.Go a
// drop-in replacement for `go` everywhere, even before init.
//
//	allstak.Go(func() { doWork() })
func Go(fn func()) {
	if fn == nil {
		return
	}
	if c := Default(); c != nil {
		c.SafeGo(nil, fn)
		return
	}
	// No client yet — still guard so the re-panic behavior is identical to the
	// instrumented path; we just can't report.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panic(r)
			}
		}()
		fn()
	}()
}

// GoCtx is Go with an explicit context for user/request/trace/breadcrumb
// enrichment on any captured panic.
func GoCtx(ctx context.Context, fn func()) {
	if fn == nil {
		return
	}
	if c := Default(); c != nil {
		c.SafeGo(ctx, fn)
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panic(r)
			}
		}()
		fn()
	}()
}
