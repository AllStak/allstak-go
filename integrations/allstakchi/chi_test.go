package allstakchi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	allstak "github.com/allstak-io/allstak-go"
	"github.com/allstak-io/allstak-go/integrations/allstakchi"
)

// recordingTransport collects payloads in memory so tests can assert on
// the exact wire shape without spinning up an HTTP server.
type recordingTransport struct {
	mu       sync.Mutex
	requests []allstak.HTTPRequestItem
	errors   []allstak.ErrorPayload
}

func (r *recordingTransport) send(_ context.Context, path string, payload any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch path {
	case "/ingest/v1/http-requests":
		if batch, ok := payload.(allstak.HTTPRequestBatch); ok {
			r.requests = append(r.requests, batch.Requests...)
		}
	case "/ingest/v1/errors":
		if p, ok := payload.(*allstak.ErrorPayload); ok {
			r.errors = append(r.errors, *p)
		}
	}
	return nil
}

// requestsCopy returns a copy of the captured requests so the caller can
// inspect without the recording mutex.
func (r *recordingTransport) requestsCopy() []allstak.HTTPRequestItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]allstak.HTTPRequestItem, len(r.requests))
	copy(out, r.requests)
	return out
}

func newClient(t *testing.T, rt *recordingTransport, cfg allstak.Config) *allstak.Client {
	t.Helper()
	c := allstak.NewWithTransport(cfg, allstak.TransportFunc(rt.send))
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// TestChiMiddleware_CapturesInboundRequest is the smoke test for the
// integration: the wrapper is a thin re-export of allstak.Middleware but
// we still want to assert that consumers using it actually get an
// HTTPRequestItem flushed.
func TestChiMiddleware_CapturesInboundRequest(t *testing.T) {
	rt := &recordingTransport{}
	c := newClient(t, rt, allstak.Config{APIKey: "ask_test", FlushInterval: 10 * time.Millisecond})

	// Chi is not a dependency of this module — we just need an
	// http.Handler stack to exercise the middleware shape.
	mw := allstakchi.Middleware(c)
	called := atomic.Bool{}
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/widgets", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer SECRET")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http: %v", err)
	}
	_ = resp.Body.Close()
	if !called.Load() {
		t.Fatal("downstream handler was not called")
	}
	// Trace header propagation: middleware echoes a trace ID.
	if resp.Header.Get("X-AllStak-Trace-Id") == "" {
		t.Fatal("middleware should echo X-AllStak-Trace-Id")
	}

	if err := c.Flush(timedCtx(t, 2*time.Second)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	reqs := rt.requestsCopy()
	if len(reqs) != 1 {
		t.Fatalf("want 1 captured request, got %d", len(reqs))
	}
	got := reqs[0]
	if got.Direction != "inbound" {
		t.Errorf("direction = %q, want inbound", got.Direction)
	}
	if got.Method != "POST" {
		t.Errorf("method = %q", got.Method)
	}
	if got.Path != "/widgets" {
		t.Errorf("path = %q", got.Path)
	}
	if got.StatusCode != 201 {
		t.Errorf("status = %d", got.StatusCode)
	}
	// Headers/bodies are off by default.
	if got.RequestHeaders != "" {
		t.Errorf("RequestHeaders captured by default: %q", got.RequestHeaders)
	}
	if got.RequestBody != "" {
		t.Errorf("RequestBody captured by default: %q", got.RequestBody)
	}
}

// TestChiMiddleware_RecoversPanicAndCaptures verifies panic safety: the
// scheduler/HTTP server must not be taken down and a fatal error should
// be enqueued.
func TestChiMiddleware_RecoversPanicAndCaptures(t *testing.T) {
	rt := &recordingTransport{}
	c := newClient(t, rt, allstak.Config{APIKey: "ask_test", FlushInterval: 10 * time.Millisecond})

	mw := allstakchi.Middleware(c)
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(errors.New("boom"))
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Make sure the test driver itself doesn't panic.
	resp, err := http.Get(srv.URL + "/p")
	if err != nil {
		t.Fatalf("http: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}

	if err := c.Flush(timedCtx(t, 2*time.Second)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	rt.mu.Lock()
	errCount := len(rt.errors)
	rt.mu.Unlock()
	if errCount == 0 {
		t.Fatal("expected at least one captured error after panic")
	}
}

// TestChiMiddleware_HeaderRedactionOptIn verifies that turning on header
// capture redacts sensitive values even through the Chi adapter.
func TestChiMiddleware_HeaderRedactionOptIn(t *testing.T) {
	rt := &recordingTransport{}
	c := newClient(t, rt, allstak.Config{
		APIKey:                "ask_test",
		FlushInterval:         10 * time.Millisecond,
		CaptureRequestHeaders: true,
	})

	mw := allstakchi.Middleware(c)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/p", nil)
	req.Header.Set("Authorization", "Bearer SECRET")
	req.Header.Set("X-Trace-Id", "tr-1")
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()

	if err := c.Flush(timedCtx(t, 2*time.Second)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	reqs := rt.requestsCopy()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	if !strings.Contains(reqs[0].RequestHeaders, "Authorization: "+allstak.Redacted) {
		t.Errorf("Authorization not redacted: %q", reqs[0].RequestHeaders)
	}
	if !strings.Contains(reqs[0].RequestHeaders, "X-Trace-Id: tr-1") {
		t.Errorf("X-Trace-Id missing: %q", reqs[0].RequestHeaders)
	}
}

func timedCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
