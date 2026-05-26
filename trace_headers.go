package allstak

import (
	"net/http"
	"strings"
)

// TraceHeaders carries inbound distributed-tracing identifiers resolved
// from W3C and AllStak-compatible headers.
type TraceHeaders struct {
	TraceID      string
	ParentSpanID string
	RequestID    string
}

// TraceHeadersFromRequest extracts trace/request identifiers from a request.
// W3C traceparent wins for trace/span adoption; x-request-id remains the
// stable request correlation id and is generated independently if absent.
func TraceHeadersFromRequest(r *http.Request) TraceHeaders {
	h := TraceHeaders{
		TraceID:      traceIDFromTraceparent(r.Header.Get("traceparent")),
		ParentSpanID: parentSpanIDFromTraceparent(r.Header.Get("traceparent")),
		RequestID:    firstHeader(r, "X-Request-Id", "X-AllStak-Request-Id"),
	}
	if h.TraceID == "" {
		h.TraceID = firstHeader(r, "X-AllStak-Trace-Id", "X-Trace-Id")
	}
	if h.ParentSpanID == "" {
		h.ParentSpanID = firstHeader(r, "X-AllStak-Span-Id", "X-Span-Id")
	}
	if h.TraceID == "" {
		h.TraceID = NewTraceID()
	}
	if h.RequestID == "" {
		h.RequestID = NewTraceID()
	}
	return h
}

func firstHeader(r *http.Request, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func traceIDFromTraceparent(header string) string {
	parts := strings.Split(strings.TrimSpace(header), "-")
	if len(parts) >= 2 && len(parts[1]) == 32 {
		return parts[1]
	}
	return ""
}

func parentSpanIDFromTraceparent(header string) string {
	parts := strings.Split(strings.TrimSpace(header), "-")
	if len(parts) >= 3 && len(parts[2]) == 16 {
		return parts[2]
	}
	return ""
}

// AllStakBaggage returns the SDK-owned W3C baggage members for correlation.
func AllStakBaggage(traceID, requestID, spanID string) string {
	parts := []string{}
	if traceID != "" {
		parts = append(parts, "allstak-trace_id="+traceID)
	}
	if requestID != "" {
		parts = append(parts, "allstak-request_id="+requestID)
	}
	if spanID != "" {
		parts = append(parts, "allstak-span_id="+spanID)
	}
	return strings.Join(parts, ",")
}

// MergeBaggage preserves caller/vendor baggage and replaces SDK-owned members.
func MergeBaggage(existing, traceID, requestID, spanID string) string {
	parts := []string{}
	for _, part := range strings.Split(existing, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" || strings.HasPrefix(strings.ToLower(trimmed), "allstak-") {
			continue
		}
		parts = append(parts, trimmed)
	}
	if baggage := AllStakBaggage(traceID, requestID, spanID); baggage != "" {
		parts = append(parts, strings.Split(baggage, ",")...)
	}
	return strings.Join(parts, ",")
}

// traceparentFlags returns the W3C trace-flags byte for the sampled decision:
// "01" when sampled, "00" when not. See
// https://www.w3.org/TR/trace-context/#trace-flags.
func traceparentFlags(sampled bool) string {
	if sampled {
		return "01"
	}
	return "00"
}

// formatTraceparent builds a W3C `traceparent` value with the given sampled
// flag. version is fixed at "00".
func formatTraceparent(traceID, spanID string, sampled bool) string {
	return "00-" + traceID + "-" + spanID + "-" + traceparentFlags(sampled)
}

// SetTraceResponseHeaders stamps correlation headers onto an HTTP response.
// The emitted traceparent is marked sampled ("-01"); use
// SetTraceResponseHeadersSampled to reflect a not-sampled decision.
func SetTraceResponseHeaders(h http.Header, traceID, requestID, spanID string) {
	SetTraceResponseHeadersSampled(h, traceID, requestID, spanID, true)
}

// SetTraceResponseHeadersSampled is SetTraceResponseHeaders with an explicit
// sampled decision controlling the traceparent trace-flags ("-01" sampled,
// "-00" not sampled).
func SetTraceResponseHeadersSampled(h http.Header, traceID, requestID, spanID string, sampled bool) {
	h.Set("X-AllStak-Trace-Id", traceID)
	if requestID != "" {
		h.Set("X-AllStak-Request-Id", requestID)
	}
	if spanID != "" {
		h.Set("X-AllStak-Span-Id", spanID)
		h.Set("traceparent", formatTraceparent(traceID, spanID, sampled))
	}
	h.Set("baggage", MergeBaggage(h.Get("baggage"), traceID, requestID, spanID))
	h.Set("AllStak-Baggage", AllStakBaggage(traceID, requestID, spanID))
}

// SetTraceRequestHeaders stamps correlation headers onto an outbound request.
// The sampled flag in the emitted traceparent is taken from the request's
// SpanContext when present (so a not-sampled trace propagates "-00"); absent a
// span context it defaults to sampled ("-01").
func SetTraceRequestHeaders(r *http.Request, traceID, requestID, spanID string) {
	sampled := true
	if sc := SpanFromContext(r.Context()); sc != nil {
		sampled = sc.Sampled
	}
	SetTraceRequestHeadersSampled(r, traceID, requestID, spanID, sampled)
}

// SetTraceRequestHeadersSampled is SetTraceRequestHeaders with an explicit
// sampled decision controlling the traceparent trace-flags.
func SetTraceRequestHeadersSampled(r *http.Request, traceID, requestID, spanID string, sampled bool) {
	r.Header.Set("X-AllStak-Trace-Id", traceID)
	if requestID != "" {
		r.Header.Set("X-AllStak-Request-Id", requestID)
	}
	if spanID != "" {
		r.Header.Set("X-AllStak-Span-Id", spanID)
		r.Header.Set("traceparent", formatTraceparent(traceID, spanID, sampled))
	}
	r.Header.Set("baggage", MergeBaggage(r.Header.Get("baggage"), traceID, requestID, spanID))
	r.Header.Set("AllStak-Baggage", AllStakBaggage(traceID, requestID, spanID))
}
