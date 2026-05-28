package allstak

import (
	"encoding/json"
	"reflect"
	"strings"
)

// PII / secret scrubbing for outbound payloads.
//
// This mirrors the semantics of the Python SDK's src/allstak/sanitize.py and
// the Java SDK's DataMasker: a recursive walk over user-supplied maps/slices
// using a case-insensitive substring match on map KEYS against a denylist.
// Matched values are replaced with the sentinel string "[REDACTED]"; keys are
// preserved. Non-map values and non-sensitive keys are passed through.
//
// Scrubbing is FAIL-OPEN by design: it must never panic or drop an event. The
// walk caps recursion depth and tracks visited containers so cyclic or
// pathologically deep input cannot hang or blow the stack. It always returns a
// sanitized copy and never mutates the caller's structures.

// redactedValue is the sentinel substituted for any value under a sensitive key.
const redactedValue = "[REDACTED]"

// maxScrubDepth caps recursion so a deeply nested (or cyclic) payload can never
// hang or overflow the stack. Anything beyond this depth is replaced wholesale
// with the sentinel rather than walked further.
const maxScrubDepth = 16

// sensitiveKeyDenylist is the canonical case-insensitive substring denylist,
// matching the other AllStak SDKs. Entries are compared in lower-case against
// the lower-cased map key.
var sensitiveKeyDenylist = []string{
	"password",
	"passwd",
	"pwd",
	"secret",
	"client_secret",
	"token",
	"access_token",
	"refresh_token",
	"x-auth-token",
	"api_key",
	"apikey",
	"x-api-key",
	"authorization",
	"auth",
	"cookie",
	"set-cookie",
	"session",
	"sessionid",
	"sessiontoken",
	"jwt",
	"credit_card",
	"cardnumber",
	"card_number",
	"cvv",
	"ssn",
	"private_key",
}

// isSensitiveKey reports whether key matches any denylist term as a
// case-insensitive substring. Pure and allocation-light.
func isSensitiveKey(key string) bool {
	lk := strings.ToLower(key)
	for _, term := range sensitiveKeyDenylist {
		if strings.Contains(lk, term) {
			return true
		}
	}
	return false
}

// scrubMap returns a sanitized deep copy of m. The original map is never
// mutated. Safe to call with a nil map (returns nil).
func scrubMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out, _ := scrubValue(m, 0, make(map[uintptr]struct{})).(map[string]any)
	return out
}

// scrubValue recursively walks value, redacting values under sensitive keys.
// depth guards against runaway recursion; seen guards against cycles among
// reference containers (maps/slices). It returns a sanitized copy and never
// panics on the shapes it handles.
func scrubValue(value any, depth int, seen map[uintptr]struct{}) any {
	if depth >= maxScrubDepth {
		return redactedValue
	}

	switch v := value.(type) {
	case map[string]any:
		if ptr := containerPtr(v); ptr != 0 {
			if _, ok := seen[ptr]; ok {
				return redactedValue // cycle guard
			}
			seen[ptr] = struct{}{}
			defer delete(seen, ptr)
		}
		out := make(map[string]any, len(v))
		for k, child := range v {
			if isSensitiveKey(k) {
				out[k] = redactedValue
				continue
			}
			out[k] = scrubValue(child, depth+1, seen)
		}
		return out

	case map[string]string:
		out := make(map[string]string, len(v))
		for k, child := range v {
			if isSensitiveKey(k) {
				out[k] = redactedValue
				continue
			}
			out[k] = child
		}
		return out

	case []any:
		if ptr := containerPtr(v); ptr != 0 {
			if _, ok := seen[ptr]; ok {
				return redactedValue // cycle guard
			}
			seen[ptr] = struct{}{}
			defer delete(seen, ptr)
		}
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = scrubValue(child, depth+1, seen)
		}
		return out

	default:
		// Primitive or unrecognized type — pass through unchanged.
		return value
	}
}

// marshalScrubbed runs payload through the same PII sanitizer the wire
// transport uses, then marshals it to JSON. It is the single helper for any
// caller that needs the EXACT scrubbed bytes the transport would send — most
// importantly the offline spool, which must persist only already-scrubbed data
// to disk (never raw secrets). Because scrubbing is idempotent, calling this in
// addition to the transport's own scrub is harmless. Fail-open: scrubbing
// cannot panic (scrubPayloadSafe recovers); a marshal error is surfaced so the
// caller can skip persisting rather than write garbage.
func marshalScrubbed(payload any) ([]byte, error) {
	return json.Marshal(scrubPayloadSafe(payload))
}

// scrubPayloadSafe wraps scrubPayload with a recover so a bug in scrubbing can
// never panic the transport or cause an event to be dropped. On any panic it
// returns the original, unscrubbed payload (fail-open).
func scrubPayloadSafe(payload any) (out any) {
	out = payload
	defer func() {
		if r := recover(); r != nil {
			out = payload
		}
	}()
	return scrubPayload(payload)
}

// scrubPayload sanitizes the user-supplied map fields of a known ingest
// payload before it is marshalled to the wire. It is the single scrubbing
// chokepoint, invoked once per send. It returns a value to marshal in place of
// the original.
//
// Only the free-form maps that can carry caller data are scrubbed
// (Metadata, Breadcrumb.Data, Span.Tags). Fixed wire fields are left alone.
// Scrubbing never mutates the caller's structures: each scrubbed payload is a
// shallow copy with freshly scrubbed maps. Fail-open: any unrecognized payload
// type is returned untouched.
func scrubPayload(payload any) any {
	switch p := payload.(type) {
	case *ErrorPayload:
		if p == nil {
			return payload
		}
		cp := *p
		cp.Metadata = scrubMap(cp.Metadata)
		if len(cp.Breadcrumbs) > 0 {
			bcs := make([]Breadcrumb, len(cp.Breadcrumbs))
			copy(bcs, cp.Breadcrumbs)
			for i := range bcs {
				bcs[i].Data = scrubMap(bcs[i].Data)
			}
			cp.Breadcrumbs = bcs
		}
		return &cp

	case *LogPayload:
		if p == nil {
			return payload
		}
		cp := *p
		cp.Metadata = scrubMap(cp.Metadata)
		return &cp

	case HTTPRequestBatch:
		if len(p.Requests) == 0 {
			return payload
		}
		items := make([]HTTPRequestItem, len(p.Requests))
		copy(items, p.Requests)
		for i := range items {
			items[i].Metadata = scrubMap(items[i].Metadata)
		}
		return HTTPRequestBatch{Requests: items}

	case SpanBatch:
		if len(p.Spans) == 0 {
			return payload
		}
		items := make([]SpanItem, len(p.Spans))
		copy(items, p.Spans)
		for i := range items {
			if m, ok := scrubValue(items[i].Tags, 0, make(map[uintptr]struct{})).(map[string]string); ok {
				items[i].Tags = m
			}
		}
		return SpanBatch{Spans: items}

	default:
		// DBQueryBatch, HeartbeatPayload, and anything else carry no
		// caller-controlled free-form maps — pass through untouched.
		return payload
	}
}

// containerPtr returns a stable identity pointer for a map or slice header so
// the walk can detect cycles. Returns 0 when no meaningful pointer exists
// (e.g. a nil/empty container), in which case the depth cap is the backstop.
func containerPtr(v any) uintptr {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map, reflect.Slice:
		return rv.Pointer()
	default:
		return 0
	}
}
