package allstak

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRedactor_DefaultDenyList(t *testing.T) {
	r := NewRedactor(nil)
	cases := []string{
		"Authorization", "authorization", "AUTHORIZATION",
		"Proxy-Authorization",
		"Cookie", "Set-Cookie",
		"X-API-Key", "X-Auth-Token", "X-Access-Token", "X-AllStak-Key",
		"refresh_token", "access-token", "id.token",
		"stripe_api_key", "stripe-api-key", "STRIPE_API_KEY",
		"user_password", "user.passwd",
		"client_secret",
		"my_session_id", "session-id",
		"csrf", "x-csrf",
	}
	for _, k := range cases {
		if !r.IsSensitiveKey(k) {
			t.Errorf("expected %q to be sensitive", k)
		}
	}
	negatives := []string{"X-Trace-Id", "User-Agent", "Content-Type", "http.method", "user.id", "x-allstak-request-id"}
	for _, k := range negatives {
		if r.IsSensitiveKey(k) {
			t.Errorf("expected %q to be public", k)
		}
	}
}

func TestRedactor_CustomPatternsStringAndRegex(t *testing.T) {
	r := NewRedactor([]string{"internal_id", `(?i)^x-internal-`})
	if !r.IsSensitiveKey("internal_id") {
		t.Errorf("custom substring should match")
	}
	if !r.IsSensitiveKey("X-Internal-Audit") {
		t.Errorf("custom regex should match")
	}
	if r.IsSensitiveKey("public_id") {
		t.Errorf("public_id should not match")
	}
}

func TestRedactor_InvalidPatternsAreSkipped(t *testing.T) {
	// Invalid regex must not panic; default deny-list still works.
	r := NewRedactor([]string{`[invalid(`})
	if !r.IsSensitiveKey("Authorization") {
		t.Errorf("invalid pattern must not break the default deny-list")
	}
}

func TestRedactor_NilRedactor(t *testing.T) {
	var r *Redactor
	// Defaults still applied via the package-level deny-list… no: nil
	// receiver returns false for everything except default checks via
	// IsSensitiveKey's nil-guard. Verify the helper at least does not
	// panic.
	defer func() {
		if x := recover(); x != nil {
			t.Fatalf("nil Redactor.IsSensitiveKey should not panic: %v", x)
		}
	}()
	_ = r.IsSensitiveKey("Authorization")
}

func TestRedactor_RedactMetadata_TopLevel(t *testing.T) {
	r := NewRedactor(nil)
	in := map[string]any{
		"authorization": "Bearer abc",
		"cookie":        "session=xyz",
		"http.method":   "GET",
		"user.id":       "u-42",
	}
	out := r.RedactMetadata(in)
	if out["authorization"] != Redacted {
		t.Errorf("expected authorization redacted, got %v", out["authorization"])
	}
	if out["cookie"] != Redacted {
		t.Errorf("expected cookie redacted, got %v", out["cookie"])
	}
	if out["http.method"] != "GET" {
		t.Errorf("expected http.method preserved, got %v", out["http.method"])
	}
	// Input must not be mutated.
	if in["authorization"] != "Bearer abc" {
		t.Errorf("input map was mutated")
	}
}

func TestRedactor_RedactMetadata_Nested(t *testing.T) {
	r := NewRedactor(nil)
	in := map[string]any{
		"http": map[string]any{
			"headers": map[string]any{
				"Authorization": "Bearer abc",
				"Content-Type":  "application/json",
			},
		},
		"user": map[string]string{
			"password": "p",
			"id":       "u-42",
		},
		"items": []any{
			map[string]any{"api_key": "k", "name": "x"},
		},
	}
	out := r.RedactMetadata(in)
	headers := out["http"].(map[string]any)["headers"].(map[string]any)
	if headers["Authorization"] != Redacted {
		t.Errorf("nested authorization not redacted")
	}
	if headers["Content-Type"] != "application/json" {
		t.Errorf("nested non-sensitive value mutated")
	}
	user := out["user"].(map[string]string)
	if user["password"] != Redacted {
		t.Errorf("nested map[string]string password not redacted")
	}
	if user["id"] != "u-42" {
		t.Errorf("user id should be preserved")
	}
	first := out["items"].([]any)[0].(map[string]any)
	if first["api_key"] != Redacted {
		t.Errorf("slice-of-map api_key not redacted")
	}
}

func TestRedactor_RedactHeaders_Order_And_MultiValue(t *testing.T) {
	r := NewRedactor(nil)
	h := http.Header{}
	h.Add("Authorization", "Bearer xyz")
	h.Add("X-Trace-Id", "tr-1")
	h.Add("Set-Cookie", "session=a")
	h.Add("Set-Cookie", "csrf=b")
	h.Add("Content-Type", "application/json")

	got := r.RedactHeaders(h)
	// Sensitive: every Set-Cookie value must be replaced.
	if !strings.Contains(got, "Set-Cookie: "+Redacted) {
		t.Errorf("Set-Cookie not redacted: %q", got)
	}
	if !strings.Contains(got, "Authorization: "+Redacted) {
		t.Errorf("Authorization not redacted: %q", got)
	}
	if !strings.Contains(got, "Content-Type: application/json") {
		t.Errorf("Content-Type lost: %q", got)
	}
	// Stable case-insensitive order.
	authIdx := strings.Index(got, "Authorization")
	traceIdx := strings.Index(got, "X-Trace-Id")
	if authIdx > traceIdx {
		t.Errorf("expected Authorization to sort before X-Trace-Id: %q", got)
	}
}

// TestCaptureError_RedactsCallerMetadata verifies sensitive caller metadata
// is replaced by the time the payload hits the transport.
func TestCaptureError_RedactsCallerMetadata(t *testing.T) {
	var mu sync.Mutex
	captured := map[string]any{}
	tf := TransportFunc(func(_ context.Context, path string, payload any) error {
		if path != pathErrors {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if p, ok := payload.(*ErrorPayload); ok {
			for k, v := range p.Metadata {
				captured[k] = v
			}
		}
		return nil
	})
	c := NewWithTransport(Config{APIKey: "ask_test", FlushInterval: 10 * time.Millisecond}, tf)
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	c.CaptureError(ErrorPayload{
		ExceptionClass: "BoomError",
		Message:        "boom",
		Metadata: map[string]any{
			"authorization":  "Bearer SECRET",
			"customer_email": "x@y.com",
			"stripe_api_key": "sk_live_abc",
			"nested": map[string]any{
				"password": "p",
			},
		},
	})

	if err := c.Flush(timedCtx(t, 2*time.Second)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if captured["authorization"] != Redacted {
		t.Errorf("authorization not redacted: %v", captured["authorization"])
	}
	if captured["stripe_api_key"] != Redacted {
		t.Errorf("stripe_api_key not redacted: %v", captured["stripe_api_key"])
	}
	nested, _ := captured["nested"].(map[string]any)
	if nested["password"] != Redacted {
		t.Errorf("nested password not redacted: %v", nested)
	}
	if captured["customer_email"] != "x@y.com" {
		t.Errorf("non-sensitive metadata altered: %v", captured["customer_email"])
	}
}

// TestCaptureHTTPRequest_DoesNotCaptureHeadersByDefault verifies safe
// defaults: headers/bodies are off and never populated.
func TestCaptureHTTPRequest_DoesNotCaptureHeadersByDefault(t *testing.T) {
	var got HTTPRequestItem
	var mu sync.Mutex
	tf := TransportFunc(func(_ context.Context, path string, payload any) error {
		if path != pathHTTPRequests {
			return nil
		}
		if batch, ok := payload.(HTTPRequestBatch); ok && len(batch.Requests) > 0 {
			mu.Lock()
			got = batch.Requests[0]
			mu.Unlock()
		}
		return nil
	})
	c := NewWithTransport(Config{APIKey: "ask_test", FlushInterval: 10 * time.Millisecond}, tf)
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	srv := httptest.NewServer(Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(204)
	})))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/p", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=xyz")
	_, _ = http.DefaultClient.Do(req)
	if err := c.Flush(timedCtx(t, 2*time.Second)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.RequestHeaders != "" {
		t.Errorf("RequestHeaders captured by default: %q", got.RequestHeaders)
	}
	if got.ResponseHeaders != "" {
		t.Errorf("ResponseHeaders captured by default: %q", got.ResponseHeaders)
	}
	if got.RequestBody != "" || got.ResponseBody != "" {
		t.Errorf("bodies captured by default: %q / %q", got.RequestBody, got.ResponseBody)
	}
}

// TestCaptureHTTPRequest_HeaderRedactionWhenEnabled verifies that opting in
// to header capture redacts sensitive values.
func TestCaptureHTTPRequest_HeaderRedactionWhenEnabled(t *testing.T) {
	var got HTTPRequestItem
	var mu sync.Mutex
	tf := TransportFunc(func(_ context.Context, path string, payload any) error {
		if path != pathHTTPRequests {
			return nil
		}
		if batch, ok := payload.(HTTPRequestBatch); ok && len(batch.Requests) > 0 {
			mu.Lock()
			got = batch.Requests[0]
			mu.Unlock()
		}
		return nil
	})
	c := NewWithTransport(Config{
		APIKey:                 "ask_test",
		FlushInterval:          10 * time.Millisecond,
		CaptureRequestHeaders:  true,
		CaptureResponseHeaders: true,
	}, tf)
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	srv := httptest.NewServer(Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "session=xyz")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(204)
	})))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/p", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=xyz")
	req.Header.Set("X-Trace-Id", "tr-1")
	_, _ = http.DefaultClient.Do(req)
	if err := c.Flush(timedCtx(t, 2*time.Second)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(got.RequestHeaders, "Authorization: "+Redacted) {
		t.Errorf("inbound Authorization not redacted: %q", got.RequestHeaders)
	}
	if !strings.Contains(got.RequestHeaders, "Cookie: "+Redacted) {
		t.Errorf("inbound Cookie not redacted: %q", got.RequestHeaders)
	}
	if !strings.Contains(got.RequestHeaders, "X-Trace-Id: tr-1") {
		t.Errorf("non-sensitive header missing: %q", got.RequestHeaders)
	}
	if !strings.Contains(got.ResponseHeaders, "Set-Cookie: "+Redacted) {
		t.Errorf("response Set-Cookie not redacted: %q", got.ResponseHeaders)
	}
}

// TestCaptureSpan_RedactsTags verifies span Tags are redacted.
func TestCaptureSpan_RedactsTags(t *testing.T) {
	var got SpanItem
	var mu sync.Mutex
	tf := TransportFunc(func(_ context.Context, path string, payload any) error {
		if path != pathSpans {
			return nil
		}
		if batch, ok := payload.(SpanBatch); ok && len(batch.Spans) > 0 {
			mu.Lock()
			got = batch.Spans[0]
			mu.Unlock()
		}
		return nil
	})
	c := NewWithTransport(Config{APIKey: "ask_test", FlushInterval: 10 * time.Millisecond}, tf)
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	c.CaptureSpan(SpanItem{
		TraceID:    "t1",
		SpanID:     "s1",
		Operation:  "op",
		DurationMs: 1,
		Tags: map[string]string{
			"http.method":   "GET",
			"authorization": "Bearer xyz",
			"api_key":       "k",
		},
	})
	if err := c.Flush(timedCtx(t, 2*time.Second)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.Tags["authorization"] != Redacted {
		t.Errorf("span tag authorization not redacted: %v", got.Tags["authorization"])
	}
	if got.Tags["api_key"] != Redacted {
		t.Errorf("span tag api_key not redacted: %v", got.Tags["api_key"])
	}
	if got.Tags["http.method"] != "GET" {
		t.Errorf("span tag http.method altered: %v", got.Tags["http.method"])
	}
}

// TestCaptureLog_RedactsMetadata verifies log metadata redaction.
func TestCaptureLog_RedactsMetadata(t *testing.T) {
	var got LogPayload
	var mu sync.Mutex
	tf := TransportFunc(func(_ context.Context, path string, payload any) error {
		if path != pathLogs {
			return nil
		}
		if p, ok := payload.(*LogPayload); ok {
			mu.Lock()
			got = *p
			mu.Unlock()
		}
		return nil
	})
	c := NewWithTransport(Config{APIKey: "ask_test", FlushInterval: 10 * time.Millisecond}, tf)
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	c.CaptureLog(LogPayload{
		Level:   "info",
		Message: "user logged in",
		Metadata: map[string]any{
			"user.id":  "u-42",
			"password": "p",
		},
	})
	if err := c.Flush(timedCtx(t, 2*time.Second)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.Metadata["password"] != Redacted {
		t.Errorf("log metadata password not redacted: %v", got.Metadata["password"])
	}
	if got.Metadata["user.id"] != "u-42" {
		t.Errorf("log metadata user.id altered")
	}
}

// TestCustomRedactKeysFromConfig verifies extra patterns from Config.
func TestCustomRedactKeysFromConfig(t *testing.T) {
	var got map[string]any
	var mu sync.Mutex
	tf := TransportFunc(func(_ context.Context, path string, payload any) error {
		if path != pathErrors {
			return nil
		}
		if p, ok := payload.(*ErrorPayload); ok {
			mu.Lock()
			got = p.Metadata
			mu.Unlock()
		}
		return nil
	})
	c := NewWithTransport(Config{
		APIKey:        "ask_test",
		FlushInterval: 10 * time.Millisecond,
		RedactKeys:    []string{"internal_id"},
	}, tf)
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	c.CaptureError(ErrorPayload{
		ExceptionClass: "E",
		Message:        "m",
		Metadata:       map[string]any{"internal_id": "x", "public_id": "y"},
	})
	if err := c.Flush(timedCtx(t, 2*time.Second)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got["internal_id"] != Redacted {
		t.Errorf("custom redact key not honored: %v", got["internal_id"])
	}
	if got["public_id"] != "y" {
		t.Errorf("public_id altered: %v", got["public_id"])
	}
}

func timedCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
