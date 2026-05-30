package allstak

import (
	"strings"
	"testing"
	"time"
)

func TestScrubMapRedactsDenylistedKeys(t *testing.T) {
	in := map[string]any{
		"password":      "hunter2",
		"Authorization": "Bearer abc",
		"API_KEY":       "ask_live_123",
		"Set-Cookie":    "sid=xyz",
		"jwt":           "eyJ...",
		"username":      "alice", // not sensitive
		"count":         42,      // not sensitive, non-string
	}
	out := scrubMap(in)

	for _, k := range []string{"password", "Authorization", "API_KEY", "Set-Cookie", "jwt"} {
		if out[k] != redactedValue {
			t.Errorf("key %q = %v, want %q", k, out[k], redactedValue)
		}
	}
	if out["username"] != "alice" {
		t.Errorf("non-sensitive key username = %v, want alice", out["username"])
	}
	if out["count"] != 42 {
		t.Errorf("non-sensitive key count = %v, want 42", out["count"])
	}
}

func TestScrubMapNestedRedaction(t *testing.T) {
	in := map[string]any{
		"user": map[string]any{
			"id":       "u1",
			"password": "secret",
			"prefs": map[string]any{
				"theme":         "dark",
				"session_token": "tok",
			},
		},
		"items": []any{
			map[string]any{"cvv": "123", "name": "card"},
			"plain",
		},
	}
	out := scrubMap(in)

	user := out["user"].(map[string]any)
	if user["id"] != "u1" {
		t.Errorf("user.id = %v, want u1", user["id"])
	}
	if user["password"] != redactedValue {
		t.Errorf("user.password = %v, want redacted", user["password"])
	}
	prefs := user["prefs"].(map[string]any)
	if prefs["theme"] != "dark" {
		t.Errorf("prefs.theme = %v, want dark", prefs["theme"])
	}
	if prefs["session_token"] != redactedValue {
		t.Errorf("prefs.session_token = %v, want redacted", prefs["session_token"])
	}

	items := out["items"].([]any)
	first := items[0].(map[string]any)
	if first["cvv"] != redactedValue {
		t.Errorf("items[0].cvv = %v, want redacted", first["cvv"])
	}
	if first["name"] != "card" {
		t.Errorf("items[0].name = %v, want card", first["name"])
	}
	if items[1] != "plain" {
		t.Errorf("items[1] = %v, want plain", items[1])
	}
}

func TestScrubMapDoesNotMutateInput(t *testing.T) {
	in := map[string]any{"password": "p", "ok": "v"}
	_ = scrubMap(in)
	if in["password"] != "p" {
		t.Errorf("input was mutated: password = %v", in["password"])
	}
}

func TestScrubMapNil(t *testing.T) {
	if scrubMap(nil) != nil {
		t.Errorf("scrubMap(nil) should return nil")
	}
}

func TestScrubMapCycleDoesNotHang(t *testing.T) {
	// Self-referential map. Must terminate via the cycle guard, not hang.
	m := map[string]any{"k": "v"}
	m["self"] = m

	done := make(chan struct{})
	go func() {
		out := scrubMap(m)
		// Top-level key preserved; the recursive self-reference is redacted
		// by the cycle guard.
		if out["k"] != "v" {
			t.Errorf("k = %v, want v", out["k"])
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scrubMap did not terminate on cyclic input")
	}
}

func TestScrubMapDeepInputBounded(t *testing.T) {
	// Build a nesting far deeper than maxScrubDepth. Must terminate and the
	// deepest part is collapsed to the sentinel rather than walked forever.
	root := map[string]any{}
	cur := root
	for i := 0; i < maxScrubDepth+50; i++ {
		next := map[string]any{}
		cur["child"] = next
		cur = next
	}

	done := make(chan struct{})
	go func() {
		_ = scrubMap(root)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scrubMap did not terminate on deep input")
	}
}

func TestScrubPayloadErrorMetadataAndBreadcrumbs(t *testing.T) {
	p := &ErrorPayload{
		Message:  "boom",
		Metadata: map[string]any{"password": "x", "service": "svc"},
		Breadcrumbs: []Breadcrumb{
			{Message: "step", Data: map[string]any{"token": "t", "page": "home"}},
		},
	}
	out := scrubPayload(p, keyOnlyOptions).(*ErrorPayload)

	if out.Metadata["password"] != redactedValue {
		t.Errorf("metadata.password not redacted: %v", out.Metadata["password"])
	}
	if out.Metadata["service"] != "svc" {
		t.Errorf("metadata.service changed: %v", out.Metadata["service"])
	}
	if out.Breadcrumbs[0].Data["token"] != redactedValue {
		t.Errorf("breadcrumb token not redacted: %v", out.Breadcrumbs[0].Data["token"])
	}
	if out.Breadcrumbs[0].Data["page"] != "home" {
		t.Errorf("breadcrumb page changed: %v", out.Breadcrumbs[0].Data["page"])
	}
	// Original must be untouched.
	if p.Metadata["password"] != "x" {
		t.Errorf("scrubPayload mutated caller payload")
	}
}

func TestScrubPayloadSpanTags(t *testing.T) {
	in := SpanBatch{Spans: []SpanItem{
		{Operation: "op", Tags: map[string]string{"authorization": "Bearer x", "region": "us"}},
	}}
	out := scrubPayload(in, keyOnlyOptions).(SpanBatch)
	if out.Spans[0].Tags["authorization"] != redactedValue {
		t.Errorf("span tag authorization not redacted: %v", out.Spans[0].Tags["authorization"])
	}
	if out.Spans[0].Tags["region"] != "us" {
		t.Errorf("span tag region changed: %v", out.Spans[0].Tags["region"])
	}
}

func TestScrubPayloadSafeFailOpenPassthrough(t *testing.T) {
	// An unknown payload type passes through unchanged.
	type custom struct{ X int }
	in := custom{X: 1}
	if got := scrubPayloadSafe(in, keyOnlyOptions); got != in {
		t.Errorf("scrubPayloadSafe passthrough = %v, want %v", got, in)
	}
}

// ── Value-pattern scrubbing (CC / SSN / email / IPv4) ──────────────────────

// piiOff is the wire default: value scrubbing on, PII off.
var piiOff = scrubOptions{scrubValues: true, sendDefaultPii: false}

// piiOn models a host that opted into PII via SendDefaultPii=true.
var piiOn = scrubOptions{scrubValues: true, sendDefaultPii: true}

func TestScrubStringCreditCardLuhnGate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Luhn-VALID card numbers are always redacted (regardless of PII flag).
		{"visa bare", "card 4111111111111111 ok", "card " + redactedValue + " ok"},
		{"visa spaced", "pay 4111 1111 1111 1111 now", "pay " + redactedValue + " now"},
		{"visa hyphen", "n=4111-1111-1111-1111", "n=" + redactedValue},
		{"amex 15", "amex 378282246310005 done", "amex " + redactedValue + " done"},
		// Luhn-INVALID digit runs are PRESERVED (order ids, timestamps, counters).
		{"bad luhn 16", "ref 4111111111111112 x", "ref 4111111111111112 x"},
		{"order id 16", "order 1234567890123456", "order 1234567890123456"},
		{"order id 13", "id 1234567890123 here", "id 1234567890123 here"},
		{"timestamp 14", "ts 20231005123456", "ts 20231005123456"},
		// Short digit runs (< 13) are never touched.
		{"phone-ish", "tel 5551234", "tel 5551234"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// CC is ALWAYS scrubbed — assert with PII both on and off.
			if got := scrubString(tc.in, piiOff); got != tc.want {
				t.Errorf("piiOff scrubString(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got := scrubString(tc.in, piiOn); got != tc.want {
				t.Errorf("piiOn scrubString(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestScrubStringSSNRequiresHyphens(t *testing.T) {
	// Hyphenated SSN is ALWAYS redacted (PII flag does not matter).
	if got := scrubString("ssn 123-45-6789 end", piiOff); got != "ssn "+redactedValue+" end" {
		t.Errorf("hyphenated SSN not redacted (piiOff): %q", got)
	}
	if got := scrubString("ssn 123-45-6789 end", piiOn); got != "ssn "+redactedValue+" end" {
		t.Errorf("hyphenated SSN not redacted (piiOn): %q", got)
	}
	// A BARE 9-digit number is NOT an SSN match (too ambiguous with ids). It is
	// also Luhn-checked as a CC candidate; 123456789 fails Luhn so it survives.
	if got := scrubString("num 123456789 end", piiOff); got != "num 123456789 end" {
		t.Errorf("bare 9-digit number should be preserved, got %q", got)
	}
}

func TestScrubStringEmailIPGatedByPii(t *testing.T) {
	const msg = "user bob@example.com from 192.168.1.42 failed"

	// Default (sendDefaultPii=false): email + IPv4 are redacted.
	gotOff := scrubString(msg, piiOff)
	wantOff := "user " + redactedValue + " from " + redactedValue + " failed"
	if gotOff != wantOff {
		t.Errorf("piiOff: got %q, want %q", gotOff, wantOff)
	}

	// Opted-in (sendDefaultPii=true): email + IPv4 PASS THROUGH unchanged.
	gotOn := scrubString(msg, piiOn)
	if gotOn != msg {
		t.Errorf("piiOn should preserve email/IP: got %q, want %q", gotOn, msg)
	}
}

func TestScrubStringIPv4OctetValidation(t *testing.T) {
	// A dotted-decimal version string with an out-of-range octet is NOT a valid
	// IPv4 and must be preserved even with PII scrubbing on.
	if got := scrubString("version 999.300.1.1 build", piiOff); got != "version 999.300.1.1 build" {
		t.Errorf("invalid IPv4-shaped version was corrupted: %q", got)
	}
	// A real IPv4 is redacted when PII is off.
	if got := scrubString("ip 10.0.0.255 done", piiOff); got != "ip "+redactedValue+" done" {
		t.Errorf("valid IPv4 not redacted: %q", got)
	}
}

func TestScrubStringPathologicalFailOpen(t *testing.T) {
	// A very large string is passed through untouched rather than scanned, so a
	// pathological input can never pin the wire path. It must not panic.
	huge := strings.Repeat("4111111111111111 ", (maxScrubStringLen/17)+10)
	if len(huge) <= maxScrubStringLen {
		t.Fatalf("test setup: huge string not over cap (%d)", len(huge))
	}
	got := scrubString(huge, piiOff)
	if got != huge {
		t.Errorf("oversized string should pass through unscrubbed")
	}
	// Empty string is a no-op.
	if scrubString("", piiOff) != "" {
		t.Errorf("empty string scrub changed it")
	}
}

func TestScrubPayloadValueScrubbingErrorPayload(t *testing.T) {
	p := &ErrorPayload{
		ExceptionClass: "PaymentError",
		Message:        "charge failed for card 4111111111111111 email a@b.com",
		// Stack frames / filenames must NOT be corrupted even if they look
		// digit-heavy or contain a path that resembles an IP.
		StackTrace: []string{"/srv/app/main.go:42", "handler at 192.168.0.1"},
		Frames: []Frame{
			{Filename: "/srv/app/v10.0.0.1/main.go", Function: "Charge", AbsPath: "/srv/app/v10.0.0.1/main.go", Lineno: 42},
		},
		// Explicit user (set via WithUser) must ship as-is — intentional ID.
		User: &UserContext{ID: "u-1", Email: "real-user@corp.com", IP: "203.0.113.7"},
		Metadata: map[string]any{
			"note":  "ssn 123-45-6789 leaked",
			"order": "1234567890123456", // Luhn-invalid order id — preserved
			"host":  "10.1.2.3",
		},
		Breadcrumbs: []Breadcrumb{
			{Message: "login by carol@site.io", Data: map[string]any{"src": "8.8.8.8"}},
		},
	}
	out := scrubPayload(p, piiOff).(*ErrorPayload)

	// Message: CC always redacted, email redacted (PII off).
	if strings.Contains(out.Message, "4111111111111111") {
		t.Errorf("CC not redacted in message: %q", out.Message)
	}
	if strings.Contains(out.Message, "a@b.com") {
		t.Errorf("email not redacted in message: %q", out.Message)
	}
	// Stack frames / filenames untouched (NOT corrupted).
	if out.StackTrace[0] != "/srv/app/main.go:42" || out.StackTrace[1] != "handler at 192.168.0.1" {
		t.Errorf("stack trace was corrupted: %v", out.StackTrace)
	}
	if out.Frames[0].Filename != "/srv/app/v10.0.0.1/main.go" || out.Frames[0].AbsPath != "/srv/app/v10.0.0.1/main.go" || out.Frames[0].Function != "Charge" {
		t.Errorf("frame fields corrupted: %+v", out.Frames[0])
	}
	// Explicit user NOT scrubbed.
	if out.User == nil || out.User.Email != "real-user@corp.com" || out.User.IP != "203.0.113.7" || out.User.ID != "u-1" {
		t.Errorf("explicit user was scrubbed: %+v", out.User)
	}
	// Metadata: SSN redacted, IP redacted, Luhn-invalid order id preserved.
	if note, _ := out.Metadata["note"].(string); strings.Contains(note, "123-45-6789") {
		t.Errorf("SSN not redacted in metadata: %q", note)
	}
	if out.Metadata["order"] != "1234567890123456" {
		t.Errorf("Luhn-invalid order id was corrupted: %v", out.Metadata["order"])
	}
	if out.Metadata["host"] != redactedValue {
		t.Errorf("IPv4 in metadata not redacted: %v", out.Metadata["host"])
	}
	// Breadcrumb message + nested data scrubbed.
	if strings.Contains(out.Breadcrumbs[0].Message, "carol@site.io") {
		t.Errorf("breadcrumb email not redacted: %q", out.Breadcrumbs[0].Message)
	}
	if out.Breadcrumbs[0].Data["src"] != redactedValue {
		t.Errorf("breadcrumb IPv4 not redacted: %v", out.Breadcrumbs[0].Data["src"])
	}
	// Original payload must be untouched (no caller mutation).
	if !strings.Contains(p.Message, "4111111111111111") {
		t.Errorf("scrubPayload mutated caller Message")
	}
	if p.Metadata["host"] != "10.1.2.3" {
		t.Errorf("scrubPayload mutated caller Metadata")
	}
}

func TestScrubPayloadValueScrubbingPiiOnPreservesEmailIP(t *testing.T) {
	p := &ErrorPayload{
		Message:  "from carol@site.io at 8.8.8.8 card 4111111111111111",
		Metadata: map[string]any{"host": "10.1.2.3", "ssn": "x"},
	}
	out := scrubPayload(p, piiOn).(*ErrorPayload)
	// PII on: email + IPv4 preserved.
	if !strings.Contains(out.Message, "carol@site.io") || !strings.Contains(out.Message, "8.8.8.8") {
		t.Errorf("PII-on should preserve email/IP in message: %q", out.Message)
	}
	if out.Metadata["host"] != "10.1.2.3" {
		t.Errorf("PII-on should preserve IPv4 metadata value: %v", out.Metadata["host"])
	}
	// CC still ALWAYS redacted even with PII on.
	if strings.Contains(out.Message, "4111111111111111") {
		t.Errorf("CC must be redacted even with PII on: %q", out.Message)
	}
	// Key-based redaction (key "ssn") still applies regardless of PII flag.
	if out.Metadata["ssn"] != redactedValue {
		t.Errorf("key-based redaction broke under PII on: %v", out.Metadata["ssn"])
	}
}

func TestScrubPayloadHTTPRequestBodiesAndHeaders(t *testing.T) {
	in := HTTPRequestBatch{Requests: []HTTPRequestItem{{
		Method:          "POST",
		Host:            "api.example.com",
		Path:            "/v1/orders/4111111111111111", // path-shaped; left to URL redactor, NOT value-scrubbed here
		RequestHeaders:  "X-Forwarded-For: 198.51.100.9",
		RequestBody:     `{"card":"4111111111111111","email":"buyer@shop.com"}`,
		ResponseBody:    "ok from 10.0.0.1",
		ResponseHeaders: "Server: nginx",
	}}}
	out := scrubPayload(in, piiOff).(HTTPRequestBatch)
	r := out.Requests[0]
	if strings.Contains(r.RequestBody, "4111111111111111") || strings.Contains(r.RequestBody, "buyer@shop.com") {
		t.Errorf("request body not scrubbed: %q", r.RequestBody)
	}
	if r.RequestHeaders == "X-Forwarded-For: 198.51.100.9" {
		t.Errorf("request header IPv4 not scrubbed: %q", r.RequestHeaders)
	}
	if r.ResponseBody != "ok from "+redactedValue {
		t.Errorf("response body IPv4 not scrubbed: %q", r.ResponseBody)
	}
	// Method/Host/Path are not value-scrubbed here (URL redactor owns those).
	if r.Path != "/v1/orders/4111111111111111" {
		t.Errorf("path was unexpectedly value-scrubbed: %q", r.Path)
	}
}

func TestScrubPayloadSafeValueScrubFailOpen(t *testing.T) {
	// Pathological but well-typed payload: a deeply self-referential metadata
	// map. Must terminate (cycle guard) and never panic; key "k" survives.
	m := map[string]any{"k": "v", "email": "x@y.com"}
	m["self"] = m
	p := &ErrorPayload{Message: "hi a@b.com", Metadata: m}

	done := make(chan struct{})
	go func() {
		out := scrubPayloadSafe(p, piiOff).(*ErrorPayload)
		if strings.Contains(out.Message, "a@b.com") {
			t.Errorf("message email not scrubbed: %q", out.Message)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scrubPayloadSafe did not terminate on cyclic metadata")
	}
}

func TestConfigScrubOptionsDefaultAndOverride(t *testing.T) {
	// Default (nil flag) => sendDefaultPii false.
	if opts := (Config{}).scrubOptions(); !opts.scrubValues || opts.sendDefaultPii {
		t.Errorf("default scrubOptions = %+v, want {scrubValues:true sendDefaultPii:false}", opts)
	}
	tru := true
	if opts := (Config{SendDefaultPii: &tru}).scrubOptions(); !opts.sendDefaultPii {
		t.Errorf("SendDefaultPii=true should yield sendDefaultPii=true: %+v", opts)
	}
	fls := false
	if opts := (Config{SendDefaultPii: &fls}).scrubOptions(); opts.sendDefaultPii {
		t.Errorf("SendDefaultPii=false should yield sendDefaultPii=false: %+v", opts)
	}
}

func TestResolveSendDefaultPiiEnvOverride(t *testing.T) {
	// Explicit flag always wins over env.
	tru := true
	t.Setenv(envSendDefaultPii, "false")
	if !resolveSendDefaultPii(&tru) {
		t.Errorf("explicit true must win over env false")
	}
	// Env enables when no explicit flag.
	t.Setenv(envSendDefaultPii, "1")
	if !resolveSendDefaultPii(nil) {
		t.Errorf("env=1 should enable PII passthrough")
	}
	t.Setenv(envSendDefaultPii, "off")
	if resolveSendDefaultPii(nil) {
		t.Errorf("env=off should keep secure default")
	}
	// Unset/garbage => secure default false.
	t.Setenv(envSendDefaultPii, "")
	if resolveSendDefaultPii(nil) {
		t.Errorf("unset env should default to false")
	}
}
