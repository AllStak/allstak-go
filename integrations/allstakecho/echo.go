// Package allstakecho provides AllStak middleware for the Echo web framework.
//
// Echo uses its own Context type and handlers that return an error rather
// than the stdlib http.Handler shape, so this package mirrors the behavior
// of allstak.Middleware (and the Gin integration) in Echo's native API:
//
//   - generate or reuse an incoming W3C traceparent / X-AllStak-Trace-Id
//   - attach SpanContext + RequestInfo to the request context
//   - recover panics in the handler chain and capture them as fatal errors
//   - capture errors returned from handlers
//   - emit an inbound HTTPRequestItem when the request finishes
//
// Users can still call allstak.WithUser(c.Request().Context(), ...) from
// their own auth middleware BEFORE this one to attach the authenticated
// principal to captured events.
package allstakecho

import (
	"net/http"
	"time"

	allstak "github.com/AllStak/allstak-go"
	"github.com/labstack/echo/v4"
)

// Middleware returns an echo.MiddlewareFunc that instruments every request.
//
// Usage:
//
//	e := echo.New()
//	e.Use(allstakecho.Middleware(client))
func Middleware(client *allstak.Client) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) (err error) {
			start := time.Now()
			req := c.Request()
			headers := allstak.TraceHeadersFromRequest(req)
			spanID := allstak.NewSpanID()

			ctx := allstak.WithRequestState(req.Context())
			ctx = allstak.WithRequestID(ctx, headers.RequestID)
			ctx = allstak.WithContextSpan(ctx, headers.TraceID, spanID, headers.ParentSpanID)
			ctx = allstak.WithRequestInfo(ctx, &allstak.RequestInfo{
				Method:    req.Method,
				Path:      routeTemplate(c),
				Host:      req.Host,
				UserAgent: req.UserAgent(),
			})
			c.SetRequest(req.WithContext(ctx))
			allstak.SetTraceResponseHeaders(c.Response().Header(), headers.TraceID, headers.RequestID, spanID)

			// Panic recovery: capture as fatal, ensure a 500 is sent if
			// nothing has been written yet, and always record the inbound
			// HTTPRequestItem so the request still shows up in the dashboard.
			// This mirrors the Gin integration and core net/http middleware,
			// which swallow the panic after sending a 500 rather than letting
			// it crash the server (Echo's router has no built-in recover).
			defer func() {
				if rec := recover(); rec != nil {
					client.CapturePanicValue(ctx, rec)
					if !c.Response().Committed {
						// Route the panic through Echo's HTTP error handler so
						// the response shape matches the rest of the app.
						c.Error(echo.NewHTTPError(http.StatusInternalServerError))
					}
					captureInbound(client, c, start)
				}
			}()

			err = next(c)
			if err != nil {
				// Echo handlers signal failures by returning an error. Capture
				// it as a non-fatal exception, then hand the error to Echo's
				// error handler so the recorded status code is accurate.
				client.CaptureException(ctx, err)
				c.Error(err)
				// Returning nil prevents Echo from invoking its error handler a
				// second time on the error we already handled above.
				err = nil
			}
			captureInbound(client, c, start)
			return err
		}
	}
}

// routeTemplate returns Echo's matched route template (e.g. "/users/:id")
// so dashboard grouping is stable across IDs and we avoid high-cardinality
// raw URLs. Falls back to the raw URL path when no route matched (404s).
func routeTemplate(c echo.Context) string {
	if path := c.Path(); path != "" {
		return path
	}
	return c.Request().URL.Path
}

// captureInbound records the inbound HTTPRequestItem. Pulled into a helper
// so the success, error, and panic paths share code.
func captureInbound(client *allstak.Client, c echo.Context, start time.Time) {
	req := c.Request()
	res := c.Response()

	status := res.Status
	if status == 0 {
		status = http.StatusOK
	}

	item := allstak.HTTPRequestItem{
		Direction:    "inbound",
		Method:       req.Method,
		Host:         req.Host,
		Path:         routeTemplate(c),
		StatusCode:   status,
		DurationMs:   int(time.Since(start).Milliseconds()),
		RequestSize:  int(req.ContentLength),
		ResponseSize: int(res.Size),
		Timestamp:    start.UTC().Format(time.RFC3339Nano),
	}
	item.RequestID = allstak.RequestIDFromContext(req.Context())
	if span := allstak.SpanFromContext(req.Context()); span != nil {
		item.TraceID = span.TraceID
		item.SpanID = span.SpanID
		item.ParentSpanID = span.ParentSpanID
	}
	if u := allstak.UserFromContext(req.Context()); u != nil {
		item.UserID = u.ID
	}
	// Auto-breadcrumb: record the inbound request on the request-scoped trail
	// so an error captured during the request carries the entry point. Mirrors
	// the core net/http middleware.
	allstak.AddHTTPBreadcrumb(req.Context(), "inbound", item.Method, item.Host, item.Path, item.StatusCode, item.DurationMs)
	client.CaptureHTTPRequest(item)
}
