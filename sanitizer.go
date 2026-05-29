package allstak

import (
	"encoding/json"
	"reflect"
	"regexp"
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
// On top of key-name redaction the walk applies VALUE-PATTERN scrubbing to
// free-text string values for PII that leaks into values regardless of the
// surrounding key (credit-card numbers, US SSNs, emails, IPv4 addresses),
// mirroring @sentry's data scrubbing. Value scrubbing is layered:
//
//   - ALWAYS-on (regardless of sendDefaultPii): credit-card numbers that pass
//     the Luhn checksum, and hyphenated US SSNs. These are high-risk
//     financial/identity values that are never legitimately wanted in
//     telemetry.
//   - Gated by sendDefaultPii (default false = Sentry parity): email addresses
//     and IPv4 addresses. When the host opts into PII (sendDefaultPii=true)
//     these pass through untouched.
//
// Scrubbing is FAIL-OPEN by design: it must never panic or drop an event. The
// walk caps recursion depth and tracks visited containers so cyclic or
// pathologically deep input cannot hang or blow the stack. It always returns a
// sanitized copy and never mutates the caller's structures.

// redactedValue is the sentinel substituted for any value under a sensitive key
// and for any value-pattern match within a free-text string.
const redactedValue = "[REDACTED]"

// maxScrubDepth caps recursion so a deeply nested (or cyclic) payload can never
// hang or overflow the stack. Anything beyond this depth is replaced wholesale
// with the sentinel rather than walked further.
const maxScrubDepth = 16

// maxScrubStringLen caps how long a single string value we will scan with the
// value-pattern regexes. Value scrubbing runs on the wire path, so an
// adversarial or accidental megabyte-long string must not pin a CPU core. A
// string longer than this is passed through unscrubbed by the value scrubbers
// (key-name redaction still applies to its key). 64 KiB comfortably covers
// real messages, headers, and small bodies.
const maxScrubStringLen = 64 * 1024

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

// Value-pattern regexes, compiled exactly once at package init so the wire path
// never recompiles. They are intentionally narrow to avoid corrupting
// legitimate data (over-redaction is a real failure mode):
//
//   - ccCandidateRe finds 13-19 digit runs allowing single space/hyphen
//     separators between digits. A match is only redacted if the digits pass
//     the Luhn checksum (luhnValid), so plain order ids / timestamps / large
//     counters that fail Luhn are preserved.
//   - ssnRe REQUIRES the dd d-dd-dddd hyphen layout. Bare 9-digit numbers are
//     NOT matched (too ambiguous with ids).
//   - emailRe is a standard, conservative email pattern.
//   - ipv4Re validates each octet is 0-255 so it never nukes arbitrary
//     dotted-decimal version strings outside that range.
var (
	ccCandidateRe = regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`)
	ssnRe         = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	emailRe       = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	ipv4Re        = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`)
)

// scrubOptions controls the value-pattern layer. The key-name denylist is
// always applied and is not configurable here.
type scrubOptions struct {
	// scrubValues enables value-pattern scrubbing at all. When false only
	// key-name redaction runs (used by the legacy key-only helpers/tests).
	scrubValues bool
	// sendDefaultPii, when true, DISABLES the email/IPv4 value scrubbers
	// (the host opted into PII, matching Sentry). The credit-card and SSN
	// scrubbers are ALWAYS on regardless of this flag.
	sendDefaultPii bool
}

// keyOnlyOptions is the legacy default: key-name redaction only, no value
// scanning. Used by scrubMap/scrubPayload/scrubPayloadSafe/marshalScrubbed so
// existing callers and tests keep their exact behavior. The wire path uses the
// *Opts variants with value scrubbing enabled.
var keyOnlyOptions = scrubOptions{scrubValues: false}

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

// luhnValid reports whether the digits in s (non-digits ignored) satisfy the
// Luhn checksum. Used to gate credit-card redaction so we only redact runs that
// are plausibly real card numbers and never nuke arbitrary digit runs (order
// ids, timestamps, large counters). A run with fewer than 13 digits is treated
// as invalid here; length is also bounded by the regex.
func luhnValid(s string) bool {
	sum := 0
	alt := false
	digits := 0
	// Walk right-to-left, doubling every second digit.
	for i := len(s) - 1; i >= 0; i-- {
		ch := s[i]
		if ch < '0' || ch > '9' {
			continue
		}
		d := int(ch - '0')
		digits++
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	if digits < 13 {
		return false
	}
	return sum%10 == 0
}

// scrubString applies the value-pattern scrubbers to a single free-text string
// according to opts. Order: always-on (CC then SSN), then gated (email, IPv4).
// It is fail-safe — the regexes cannot panic — and length-bounded so a huge
// string is passed through untouched rather than scanned. The CC pass only
// redacts Luhn-valid runs so legitimate non-card digit runs survive.
func scrubString(s string, opts scrubOptions) string {
	if !opts.scrubValues || s == "" {
		return s
	}
	if len(s) > maxScrubStringLen {
		// Too large to scan on the wire path — pass through. Key-name
		// redaction still covered its key upstream.
		return s
	}

	// (A) ALWAYS — credit cards (only when Luhn-valid) and hyphenated SSNs.
	out := ccCandidateRe.ReplaceAllStringFunc(s, func(m string) string {
		if luhnValid(m) {
			return redactedValue
		}
		return m
	})
	out = ssnRe.ReplaceAllString(out, redactedValue)

	// (B) Gated by sendDefaultPii — email + IPv4.
	if !opts.sendDefaultPii {
		out = emailRe.ReplaceAllString(out, redactedValue)
		out = ipv4Re.ReplaceAllString(out, redactedValue)
	}
	return out
}

// scrubMap returns a sanitized deep copy of m using key-name redaction only.
// The original map is never mutated. Safe to call with a nil map (returns nil).
// This is the legacy helper retained for existing callers/tests; the wire path
// uses scrubMapOpts.
func scrubMap(m map[string]any) map[string]any {
	return scrubMapOpts(m, keyOnlyOptions)
}

// scrubMapOpts is scrubMap with explicit options (including value-pattern
// scrubbing). The original map is never mutated.
func scrubMapOpts(m map[string]any, opts scrubOptions) map[string]any {
	if m == nil {
		return nil
	}
	out, _ := scrubValue(m, 0, make(map[uintptr]struct{}), opts).(map[string]any)
	return out
}

// scrubValue recursively walks value, redacting values under sensitive keys and
// (when opts.scrubValues is set) applying value-pattern scrubbing to free-text
// strings. depth guards against runaway recursion; seen guards against cycles
// among reference containers (maps/slices). It returns a sanitized copy and
// never panics on the shapes it handles.
func scrubValue(value any, depth int, seen map[uintptr]struct{}, opts scrubOptions) any {
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
			out[k] = scrubValue(child, depth+1, seen, opts)
		}
		return out

	case map[string]string:
		out := make(map[string]string, len(v))
		for k, child := range v {
			if isSensitiveKey(k) {
				out[k] = redactedValue
				continue
			}
			out[k] = scrubString(child, opts)
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
			out[i] = scrubValue(child, depth+1, seen, opts)
		}
		return out

	case string:
		return scrubString(v, opts)

	default:
		// Non-string primitive or unrecognized type — pass through unchanged.
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
func marshalScrubbed(payload any, opts scrubOptions) ([]byte, error) {
	return json.Marshal(scrubPayloadSafe(payload, opts))
}

// scrubPayloadSafe wraps scrubPayload with a recover so a bug in scrubbing can
// never panic the transport or cause an event to be dropped. On any panic it
// returns the original, unscrubbed payload (fail-open). Note: "fail-open" here
// means the key-name + value scrubbing on the successfully-processed maps still
// applied; a panic only forfeits whatever was mid-walk, never the whole event.
func scrubPayloadSafe(payload any, opts scrubOptions) (out any) {
	out = payload
	defer func() {
		if r := recover(); r != nil {
			out = payload
		}
	}()
	return scrubPayload(payload, opts)
}

// scrubPayload sanitizes the user-supplied fields of a known ingest payload
// before it is marshalled to the wire. It is the single scrubbing chokepoint,
// invoked once per send. It returns a value to marshal in place of the
// original.
//
// Key-name redaction applies to every free-form map (Metadata, Breadcrumb.Data,
// Span.Tags, HTTP request Metadata). Value-pattern scrubbing additionally
// applies to free-text string fields per opts: error/log/breadcrumb messages,
// captured HTTP request/response headers + bodies, and string values inside the
// free-form maps.
//
// Fields that are deliberately NOT value-scrubbed (matching Sentry):
//   - the explicit User object (user.id/email/ip are intentional identification),
//   - stack frames (filename/function/absPath), release/sdk/platform fields,
//     span operation/description, URLs/paths, and the SDK's own session id.
//
// Scrubbing never mutates the caller's structures: each scrubbed payload is a
// shallow copy with freshly scrubbed maps/fields. Fail-open: any unrecognized
// payload type is returned untouched.
func scrubPayload(payload any, opts scrubOptions) any {
	switch p := payload.(type) {
	case *ErrorPayload:
		if p == nil {
			return payload
		}
		cp := *p
		cp.Message = scrubString(cp.Message, opts)
		cp.Metadata = scrubMapOpts(cp.Metadata, opts)
		if len(cp.Breadcrumbs) > 0 {
			bcs := make([]Breadcrumb, len(cp.Breadcrumbs))
			copy(bcs, cp.Breadcrumbs)
			for i := range bcs {
				bcs[i].Message = scrubString(bcs[i].Message, opts)
				bcs[i].Data = scrubMapOpts(bcs[i].Data, opts)
			}
			cp.Breadcrumbs = bcs
		}
		// User is the EXPLICIT principal set via WithUser/setUser — intentional
		// identification that ships as-is, matching Sentry. StackTrace/Frames,
		// release/sdk/platform, traceId, requestContext (method/path/host/UA),
		// and sessionId are left untouched on purpose.
		return &cp

	case *LogPayload:
		if p == nil {
			return payload
		}
		cp := *p
		cp.Message = scrubString(cp.Message, opts)
		cp.Metadata = scrubMapOpts(cp.Metadata, opts)
		return &cp

	case HTTPRequestBatch:
		if len(p.Requests) == 0 {
			return payload
		}
		items := make([]HTTPRequestItem, len(p.Requests))
		copy(items, p.Requests)
		for i := range items {
			// Captured request/response headers + bodies are free text that
			// can leak PII. Method/Host/Path are URL-shaped and have their own
			// URL redactor elsewhere, so they are left alone here.
			items[i].RequestHeaders = scrubString(items[i].RequestHeaders, opts)
			items[i].ResponseHeaders = scrubString(items[i].ResponseHeaders, opts)
			items[i].RequestBody = scrubString(items[i].RequestBody, opts)
			items[i].ResponseBody = scrubString(items[i].ResponseBody, opts)
			items[i].Metadata = scrubMapOpts(items[i].Metadata, opts)
		}
		return HTTPRequestBatch{Requests: items}

	case SpanBatch:
		if len(p.Spans) == 0 {
			return payload
		}
		items := make([]SpanItem, len(p.Spans))
		copy(items, p.Spans)
		for i := range items {
			if m, ok := scrubValue(items[i].Tags, 0, make(map[uintptr]struct{}), opts).(map[string]string); ok {
				items[i].Tags = m
			}
			// Operation is a span NAME (low-cardinality identifier) and is left
			// alone per the redaction model. Description and Data are free-text
			// fields that can carry PII, so scrub them.
			items[i].Description = scrubString(items[i].Description, opts)
			items[i].Data = scrubString(items[i].Data, opts)
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
