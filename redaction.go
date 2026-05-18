package allstak

import (
	"net/http"
	"regexp"
	"strings"
)

// Redacted is the placeholder value substituted for any sensitive field
// before it leaves the process. Constant on purpose so tests and ingest
// can both pattern-match on it.
const Redacted = "[REDACTED]"

// defaultRedactKeyPatterns is the built-in deny-list applied to map keys
// (Metadata, tags) and HTTP header names. Patterns are matched
// case-insensitively against the full key. Suffix-style matching is used
// so e.g. "stripe_api_key" matches "api_key".
//
// Adding to this list is API-stable: callers cannot opt out of the
// defaults. To extend, set Config.RedactKeys.
var defaultRedactKeyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^authorization$`),
	regexp.MustCompile(`(?i)^proxy-authorization$`),
	regexp.MustCompile(`(?i)^cookie$`),
	regexp.MustCompile(`(?i)^set-cookie$`),
	regexp.MustCompile(`(?i)^x-api-key$`),
	regexp.MustCompile(`(?i)^x-auth-token$`),
	regexp.MustCompile(`(?i)^x-access-token$`),
	regexp.MustCompile(`(?i)^x-allstak-key$`),
	regexp.MustCompile(`(?i)(^|[._-])token$`),
	regexp.MustCompile(`(?i)(^|[._-])api[._-]?key$`),
	regexp.MustCompile(`(?i)(^|[._-])password$`),
	regexp.MustCompile(`(?i)(^|[._-])passwd$`),
	regexp.MustCompile(`(?i)(^|[._-])secret$`),
	regexp.MustCompile(`(?i)(^|[._-])session[._-]?id$`),
	regexp.MustCompile(`(?i)(^|[._-])csrf$`),
	// Added in 0.1.3 for canonical denylist parity across the SDK ecosystem.
	regexp.MustCompile(`(?i)(^|[._-])bearer$`),
	regexp.MustCompile(`(?i)(^|[._-])jwt$`),
	regexp.MustCompile(`(?i)(^|[._-])pwd$`),
	regexp.MustCompile(`(?i)(^|[._-])credit[._-]?card$`),
	regexp.MustCompile(`(?i)(^|[._-])card[._-]?number$`),
	regexp.MustCompile(`(?i)(^|[._-])cvv$`),
	regexp.MustCompile(`(?i)(^|[._-])ssn$`),
}

// Redactor decides which keys are sensitive. Zero value is usable and
// uses only the built-in deny-list.
type Redactor struct {
	extra []*regexp.Regexp
}

// NewRedactor builds a Redactor with the built-in deny-list plus the
// caller-supplied extra patterns. Strings are compiled as case-insensitive
// substring matches; regex strings starting with "(?" are treated as raw
// patterns. Invalid patterns are silently dropped (fail-safe: we never
// fail-open on a redactor error).
func NewRedactor(extra []string) *Redactor {
	out := make([]*regexp.Regexp, 0, len(extra))
	for _, p := range extra {
		if p == "" {
			continue
		}
		var compiled *regexp.Regexp
		var err error
		if strings.HasPrefix(p, "(?") || strings.HasPrefix(p, "^") {
			compiled, err = regexp.Compile(p)
		} else {
			compiled, err = regexp.Compile("(?i)" + regexp.QuoteMeta(p))
		}
		if err == nil && compiled != nil {
			out = append(out, compiled)
		}
	}
	return &Redactor{extra: out}
}

// IsSensitiveKey returns true if key matches the built-in deny-list or
// any caller-supplied extra pattern. Always case-insensitive.
func (r *Redactor) IsSensitiveKey(key string) bool {
	for _, pat := range defaultRedactKeyPatterns {
		if pat.MatchString(key) {
			return true
		}
	}
	if r == nil {
		return false
	}
	for _, pat := range r.extra {
		if pat.MatchString(key) {
			return true
		}
	}
	return false
}

// RedactMetadata returns a fresh map with sensitive values replaced by
// Redacted. The input map is never mutated. Nested map[string]any values
// are walked recursively so JSON-style payloads are fully redacted.
//
// This is the public entry point used by capture paths. It is safe to
// call with a nil map (returns nil).
func (r *Redactor) RedactMetadata(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if r.IsSensitiveKey(k) {
			out[k] = Redacted
			continue
		}
		out[k] = r.redactValue(v)
	}
	return out
}

// redactValue walks composite values without considering its own key.
// Only nested maps are descended into — slices of scalars are kept as-is.
// For slices of maps each element is processed.
func (r *Redactor) redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return r.RedactMetadata(x)
	case map[string]string:
		out := make(map[string]string, len(x))
		for k, sv := range x {
			if r.IsSensitiveKey(k) {
				out[k] = Redacted
			} else {
				out[k] = sv
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = r.redactValue(item)
		}
		return out
	}
	return v
}

// RedactStringMap is a typed convenience for Span.Tags (map[string]string)
// and similar headers-as-strings maps. Returns a fresh copy.
func (r *Redactor) RedactStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if r.IsSensitiveKey(k) {
			out[k] = Redacted
		} else {
			out[k] = v
		}
	}
	return out
}

// RedactHeaders flattens an http.Header into a comma-joined string per
// the AllStak wire format (HTTPRequestItem.RequestHeaders /
// .ResponseHeaders are single strings). Sensitive headers are emitted as
// "key: [REDACTED]". The output is sorted by key for stable test output.
//
// Header capture is OFF by default — this helper exists so opt-in capture
// paths (when Config.CaptureRequestHeaders=true) cannot accidentally
// leak credentials.
func (r *Redactor) RedactHeaders(h http.Header) string {
	if len(h) == 0 {
		return ""
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	// Stable order without importing sort to avoid pulling another dep.
	// Use a simple insertion sort — header maps are tiny.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && strings.ToLower(keys[j-1]) > strings.ToLower(keys[j]); j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(k)
		b.WriteString(": ")
		if r.IsSensitiveKey(k) {
			b.WriteString(Redacted)
			continue
		}
		b.WriteString(strings.Join(h.Values(k), ","))
	}
	return b.String()
}

// activeRedactor returns the client's redactor or a zero-value one. We
// never want a nil-deref in a capture path.
func (c *Client) activeRedactor() *Redactor {
	if c == nil || c.redactor == nil {
		return &Redactor{}
	}
	return c.redactor
}
