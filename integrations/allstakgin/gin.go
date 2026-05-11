// Package allstakgin provides AllStak middleware for the Gin web framework.
//
// Gin uses its own Context type rather than stdlib http.Handler, so this
// package mirrors the behavior of allstak.Middleware in Gin's native API:
//
//   - generate or reuse an incoming X-AllStak-Trace-Id header
//   - attach SpanContext + RequestInfo to the request context
//   - recover panics and capture them as fatal errors
//   - emit an inbound HTTPRequestItem when the request finishes
//
// Users can still call allstak.WithUser(c.Request.Context(), ...) from
// their own auth middleware BEFORE this one to attach the authenticated
// principal to captured events.
package allstakgin

import (
	"net/http"
	"time"

	allstak "github.com/AllStak/allstak-go"
	"github.com/gin-gonic/gin"
)

// Middleware returns a gin.HandlerFunc that instruments every request.
//
// Usage:
//
//	r := gin.New()
//	r.Use(allstakgin.Middleware(client))
func Middleware(client *allstak.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		traceID := c.GetHeader("X-AllStak-Trace-Id")
		if traceID == "" {
			traceID = allstak.NewTraceID()
		}
		parentSpan := c.GetHeader("X-AllStak-Span-Id")
		spanID := allstak.NewSpanID()

		ctx := allstak.WithRequestState(c.Request.Context())
		ctx = allstak.WithRequestID(ctx, traceID)
		ctx = allstak.WithContextSpan(ctx, traceID, spanID, parentSpan)
		ctx = allstak.WithRequestInfo(ctx, &allstak.RequestInfo{
			Method:    c.Request.Method,
			Path:      c.FullPath(),
			Host:      c.Request.Host,
			UserAgent: c.Request.UserAgent(),
		})
		c.Request = c.Request.WithContext(ctx)
		c.Writer.Header().Set("X-AllStak-Trace-Id", traceID)

		// Panic recovery: capture as fatal, ensure a 500 is sent if
		// nothing has been written yet, and always record the inbound
		// HTTPRequestItem so the request still shows up in the dashboard.
		defer func() {
			if rec := recover(); rec != nil {
				client.CapturePanicValue(ctx, rec)
				if !c.Writer.Written() {
					c.AbortWithStatus(http.StatusInternalServerError)
				}
				captureInbound(client, c, start)
				return
			}
			captureInbound(client, c, start)
		}()

		c.Next()
	}
}

// captureInbound records the inbound HTTPRequestItem. Pulled into a
// helper so success and panic paths share code.
func captureInbound(client *allstak.Client, c *gin.Context, start time.Time) {
	// Use the Gin-matched route pattern when available (e.g. "/users/:id")
	// so dashboard grouping is stable across IDs. Fall back to the raw
	// URL path on 404s.
	path := c.FullPath()
	if path == "" {
		path = c.Request.URL.Path
	}

	item := allstak.HTTPRequestItem{
		Direction:    "inbound",
		Method:       c.Request.Method,
		Host:         c.Request.Host,
		Path:         path,
		StatusCode:   c.Writer.Status(),
		DurationMs:   int(time.Since(start).Milliseconds()),
		RequestSize:  int(c.Request.ContentLength),
		ResponseSize: c.Writer.Size(),
		Timestamp:    start.UTC().Format(time.RFC3339Nano),
	}
	if tid, sid := allstak.TraceFromContext(c.Request.Context()); tid != "" {
		item.TraceID = tid
		item.SpanID = sid
	}
	if u := allstak.UserFromContext(c.Request.Context()); u != nil {
		item.UserID = u.ID
	}
	client.CaptureHTTPRequest(item)
}
