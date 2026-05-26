package allstak

import (
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
	out := scrubPayload(p).(*ErrorPayload)

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
	out := scrubPayload(in).(SpanBatch)
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
	if got := scrubPayloadSafe(in); got != in {
		t.Errorf("scrubPayloadSafe passthrough = %v, want %v", got, in)
	}
}
