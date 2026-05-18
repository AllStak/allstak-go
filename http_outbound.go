package allstak

import (
	"net/http"
	"time"
)

// NewTransport wraps an inner http.RoundTripper with automatic outbound
// HTTP capture. If inner is nil, http.DefaultTransport is used.
//
// Usage:
//
//	httpClient := &http.Client{
//	    Transport: allstak.NewTransport(client, nil),
//	}
//
// The wrapper records one HTTPRequestItem per RoundTrip with direction
// "outbound", the final status code (including 0 for network failures),
// duration, and the trace ID from the request context if present.
func NewTransport(client *Client, inner http.RoundTripper) http.RoundTripper {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &outboundTransport{
		client: client,
		inner:  inner,
	}
}

// outboundTransport is the wrapping RoundTripper. It is intentionally
// simple — it does not buffer request or response bodies, because doing so
// would change timing semantics and could break streaming endpoints. If
// users want body capture they should build a custom layer on top.
type outboundTransport struct {
	client *Client
	inner  http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
//
// The contract here is important: the inner RoundTripper must still
// receive the request exactly as-is, its errors must be propagated
// verbatim, and the response body must remain untouched. We only observe
// start time, end time, and the final status — nothing mutates the
// request/response pair.
func (t *outboundTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	// Propagate trace context into the outbound request headers so the
	// downstream service can link its own spans to ours if it's also
	// running the SDK. We use the same header names as OpenTelemetry's
	// W3C traceparent for forward compatibility.
	if sc := SpanFromContext(req.Context()); sc != nil && sc.TraceID != "" {
		if req.Header.Get("traceparent") == "" {
			req.Header.Set("traceparent", traceParentHeader(sc.TraceID, sc.SpanID))
		}
		if req.Header.Get("X-AllStak-Trace-Id") == "" {
			req.Header.Set("X-AllStak-Trace-Id", sc.TraceID)
		}
		requestID := RequestIDFromContext(req.Context())
		if requestID == "" {
			requestID = sc.TraceID
		}
		if req.Header.Get("X-AllStak-Request-Id") == "" {
			req.Header.Set("X-AllStak-Request-Id", requestID)
		}
		if sc.SpanID != "" {
			if req.Header.Get("X-AllStak-Span-Id") == "" {
				req.Header.Set("X-AllStak-Span-Id", sc.SpanID)
			}
		}
	}

	resp, err := t.inner.RoundTrip(req)
	elapsed := time.Since(start)

	item := HTTPRequestItem{
		Direction:  "outbound",
		Method:     req.Method,
		Host:       req.URL.Host,
		Path:       req.URL.Path,
		DurationMs: int(elapsed.Milliseconds()),
		Timestamp:  start.UTC().Format(time.RFC3339Nano),
	}

	// Fill IDs from the current context.
	if sc := SpanFromContext(req.Context()); sc != nil {
		item.TraceID = sc.TraceID
		item.SpanID = sc.SpanID
		item.ParentSpanID = sc.ParentSpanID
	}
	if rid := RequestIDFromContext(req.Context()); rid != "" {
		item.Metadata = map[string]any{"requestId": rid}
	}
	if u := UserFromContext(req.Context()); u != nil {
		item.UserID = u.ID
	}

	if err != nil {
		// Network/transport failure — use a synthetic "0" status and
		// stamp a fingerprint that the backend can use to group by
		// destination + failure class.
		item.StatusCode = 0
		item.ErrorFingerprint = "outbound:" + req.URL.Host + ":transport-error"
	} else if resp != nil {
		item.StatusCode = resp.StatusCode
		if resp.ContentLength > 0 {
			item.ResponseSize = int(resp.ContentLength)
		}
	}

	if t.client.cfg.CaptureRequestHeaders {
		item.RequestHeaders = t.client.activeRedactor().RedactHeaders(req.Header)
	}
	if t.client.cfg.CaptureResponseHeaders && resp != nil {
		item.ResponseHeaders = t.client.activeRedactor().RedactHeaders(resp.Header)
	}

	t.client.CaptureHTTPRequest(item)
	return resp, err
}
