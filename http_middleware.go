package allstak

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Middleware returns a net/http middleware that:
//
//  1. Stamps a request ID, trace ID, and SpanContext onto the request context.
//  2. Stores request info (method/path/host/user-agent) so errors captured
//     during the request can be enriched automatically.
//  3. Records an HTTPRequestItem with direction "inbound" when the request
//     finishes, including the final status code and duration.
//  4. Recovers panics, captures them as fatal errors, and returns a 500.
//
// This is the base middleware. Chi and Gin integrations are thin adapters
// that call through to this or mirror its behavior in the framework's
// native types.
//
// Usage with net/http or Chi:
//
//	mux.Handle("/api/", allstak.Middleware(client)(apiHandler))
//
// If the user is authenticated, call allstak.WithUser(ctx, &User{...}) in
// your auth middleware BEFORE this one so the request-scoped user is
// visible to any error captured downstream.
func Middleware(client *Client) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			traceID, traceParentSpan := parseTraceParent(r.Header.Get("traceparent"))
			if traceID == "" {
				traceID = r.Header.Get("X-AllStak-Trace-Id")
			}
			if traceID == "" {
				traceID = NewTraceID()
			}
			requestID := r.Header.Get("X-AllStak-Request-Id")
			if requestID == "" {
				requestID = traceID
			}
			parentSpan := r.Header.Get("X-AllStak-Span-Id")
			if parentSpan == "" {
				parentSpan = traceParentSpan
			}
			spanID := NewSpanID()

			// Install a mutable request-state bag FIRST. Downstream
			// middleware (e.g. auth) will write the resolved user into
			// this bag via WithUser, which means our deferred panic
			// handler below will still see it because the bag is a
			// pointer shared across the whole request.
			ctx := withRequestState(r.Context(), newRequestState())
			ctx = WithRequestID(ctx, requestID)
			ctx = withSpan(ctx, &SpanContext{
				TraceID:      traceID,
				SpanID:       spanID,
				ParentSpanID: parentSpan,
			})
			ctx = WithRequestInfo(ctx, &RequestInfo{
				Method:    r.Method,
				Path:      r.URL.Path,
				Host:      r.Host,
				UserAgent: r.Header.Get("User-Agent"),
			})

			// Propagate the trace ID back so the client/browser can
			// correlate its own logs.
			w.Header().Set("X-AllStak-Trace-Id", traceID)
			w.Header().Set("X-AllStak-Request-Id", requestID)
			w.Header().Set("traceparent", traceParentHeader(traceID, spanID))

			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			// Panic recovery. We capture the panic as a fatal error,
			// send a 500 response if nothing has been written yet, and
			// then rely on the normal HTTPRequestItem path to record
			// the request itself (statusCode will be 500).
			defer func() {
				if rec := recover(); rec != nil {
					client.capturePanic(ctx, rec)
					if !rw.wroteHeader {
						http.Error(rw, "internal server error", http.StatusInternalServerError)
					}
					client.captureInbound(ctx, r, rw, start)
					return
				}
				client.captureInbound(ctx, r, rw, start)
			}()

			next.ServeHTTP(rw, r.WithContext(ctx))
		})
	}
}

// captureInbound builds and enqueues the inbound HTTPRequestItem. Pulled
// into a helper so the success path and panic path share code.
func (c *Client) captureInbound(ctx context.Context, r *http.Request, rw *statusRecorder, start time.Time) {
	sc := SpanFromContext(ctx)
	item := HTTPRequestItem{
		Direction:    "inbound",
		Method:       r.Method,
		Host:         r.Host,
		Path:         r.URL.Path,
		StatusCode:   rw.status,
		DurationMs:   int(time.Since(start).Milliseconds()),
		RequestSize:  int(r.ContentLength),
		ResponseSize: rw.bytes,
		Timestamp:    start.UTC().Format(time.RFC3339Nano),
	}
	if sc != nil {
		item.TraceID = sc.TraceID
		item.SpanID = sc.SpanID
		item.ParentSpanID = sc.ParentSpanID
	}
	if rid := RequestIDFromContext(ctx); rid != "" {
		item.Metadata = map[string]any{"requestId": rid}
	}
	if u := UserFromContext(ctx); u != nil {
		item.UserID = u.ID
	}
	if c.cfg.CaptureRequestHeaders {
		item.RequestHeaders = c.activeRedactor().RedactHeaders(r.Header)
	}
	if c.cfg.CaptureResponseHeaders {
		item.ResponseHeaders = c.activeRedactor().RedactHeaders(rw.Header())
	}
	c.CaptureHTTPRequest(item)
}

// statusRecorder wraps an http.ResponseWriter to capture the final status
// code and response byte count. It also supports http.Hijacker and
// http.Flusher via conditional delegation so middleware stacks that need
// websockets or SSE still work.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Hijack passes through to the wrapped writer if it supports the
// Hijacker interface. This is required for websocket upgrades etc.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("allstak: wrapped ResponseWriter does not implement http.Hijacker")
	}
	return hj.Hijack()
}

// Flush passes through to the wrapped writer if it supports the
// Flusher interface. Needed for SSE/streaming handlers.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
