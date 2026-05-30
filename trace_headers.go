package allstak

import (
	"net/http"
	"strings"
	"unicode"
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
		h.TraceID = firstValidTraceHeader(r, "X-AllStak-Trace-Id", "X-Trace-Id")
	}
	if h.ParentSpanID == "" && h.TraceID != "" {
		h.ParentSpanID = firstValidSpanHeader(r, "X-AllStak-Span-Id", "X-Span-Id")
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
	if len(parts) == 4 && parts[0] == "00" && isHex(parts[3]) && len(parts[3]) == 2 {
		traceID := strings.ToLower(parts[1])
		if isValidTraceID(traceID) {
			return traceID
		}
	}
	return ""
}

func parentSpanIDFromTraceparent(header string) string {
	parts := strings.Split(strings.TrimSpace(header), "-")
	if len(parts) == 4 && parts[0] == "00" && isHex(parts[3]) && len(parts[3]) == 2 {
		spanID := strings.ToLower(parts[2])
		if isValidSpanID(spanID) {
			return spanID
		}
	}
	return ""
}

func firstValidTraceHeader(r *http.Request, names ...string) string {
	for _, name := range names {
		value := strings.ToLower(strings.TrimSpace(r.Header.Get(name)))
		if isValidTraceID(value) {
			return value
		}
	}
	return ""
}

func firstValidSpanHeader(r *http.Request, names ...string) string {
	for _, name := range names {
		value := strings.ToLower(strings.TrimSpace(r.Header.Get(name)))
		if isValidSpanID(value) {
			return value
		}
	}
	return ""
}

func isValidTraceID(value string) bool {
	return len(value) == 32 && isHex(value) && !allZeros(value)
}

func isValidSpanID(value string) bool {
	return len(value) == 16 && isHex(value) && !allZeros(value)
}

func isHex(value string) bool {
	for _, r := range value {
		if !unicode.Is(unicode.ASCII_Hex_Digit, r) {
			return false
		}
	}
	return value != ""
}

func allZeros(value string) bool {
	for _, r := range value {
		if r != '0' {
			return false
		}
	}
	return value != ""
}

func normalizeTraceID(traceID string) string {
	hex := hexOnly(traceID)
	var candidate string
	switch {
	case len(hex) >= 32:
		candidate = hex[:32]
	case len(hex) > 0:
		candidate = hex + strings.Repeat("0", 32-len(hex))
	}
	if isValidTraceID(candidate) {
		return candidate
	}
	return NewTraceID()
}

func normalizeSpanID(spanID string) string {
	hex := hexOnly(spanID)
	var candidate string
	switch {
	case len(hex) >= 16:
		candidate = hex[:16]
	case len(hex) > 0:
		candidate = hex + strings.Repeat("0", 16-len(hex))
	}
	if isValidSpanID(candidate) {
		return candidate
	}
	return NewSpanID()
}

func hexOnly(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.Is(unicode.ASCII_Hex_Digit, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
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
	return "00-" + normalizeTraceID(traceID) + "-" + normalizeSpanID(spanID) + "-" + traceparentFlags(sampled)
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
	wireTraceID := normalizeTraceID(traceID)
	wireSpanID := ""
	if spanID != "" {
		wireSpanID = normalizeSpanID(spanID)
	}
	h.Set("X-AllStak-Trace-Id", wireTraceID)
	if requestID != "" {
		h.Set("X-AllStak-Request-Id", requestID)
	}
	if wireSpanID != "" {
		h.Set("X-AllStak-Span-Id", wireSpanID)
		h.Set("traceparent", formatTraceparent(wireTraceID, wireSpanID, sampled))
	}
	h.Set("baggage", MergeBaggage(h.Get("baggage"), wireTraceID, requestID, wireSpanID))
	h.Set("AllStak-Baggage", AllStakBaggage(wireTraceID, requestID, wireSpanID))
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
	wireTraceID := normalizeTraceID(traceID)
	wireSpanID := ""
	if spanID != "" {
		wireSpanID = normalizeSpanID(spanID)
	}
	r.Header.Set("X-AllStak-Trace-Id", wireTraceID)
	if requestID != "" {
		r.Header.Set("X-AllStak-Request-Id", requestID)
	}
	if wireSpanID != "" {
		r.Header.Set("X-AllStak-Span-Id", wireSpanID)
		r.Header.Set("traceparent", formatTraceparent(wireTraceID, wireSpanID, sampled))
	}
	r.Header.Set("baggage", MergeBaggage(r.Header.Get("baggage"), wireTraceID, requestID, wireSpanID))
	r.Header.Set("AllStak-Baggage", AllStakBaggage(wireTraceID, requestID, wireSpanID))
}
