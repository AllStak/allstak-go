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

// SetTraceResponseHeaders stamps correlation headers onto an HTTP response.
func SetTraceResponseHeaders(h http.Header, traceID, requestID, spanID string) {
	h.Set("X-AllStak-Trace-Id", traceID)
	if requestID != "" {
		h.Set("X-AllStak-Request-Id", requestID)
	}
	if spanID != "" {
		h.Set("X-AllStak-Span-Id", spanID)
		h.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	}
	h.Set("baggage", MergeBaggage(h.Get("baggage"), traceID, requestID, spanID))
	h.Set("AllStak-Baggage", AllStakBaggage(traceID, requestID, spanID))
}

// SetTraceRequestHeaders stamps correlation headers onto an outbound request.
func SetTraceRequestHeaders(r *http.Request, traceID, requestID, spanID string) {
	r.Header.Set("X-AllStak-Trace-Id", traceID)
	if requestID != "" {
		r.Header.Set("X-AllStak-Request-Id", requestID)
	}
	if spanID != "" {
		r.Header.Set("X-AllStak-Span-Id", spanID)
		r.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	}
	r.Header.Set("baggage", MergeBaggage(r.Header.Get("baggage"), traceID, requestID, spanID))
	r.Header.Set("AllStak-Baggage", AllStakBaggage(traceID, requestID, spanID))
}
