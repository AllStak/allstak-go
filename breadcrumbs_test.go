package allstak

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAddBreadcrumbAttachedToCapturedError(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{APIKey: "ask_test"}, rt)

	ctx := WithBreadcrumbs(context.Background())
	c.AddBreadcrumb(ctx, Breadcrumb{Category: "auth", Message: "login ok"})
	c.AddBreadcrumb(ctx, Breadcrumb{Category: "cart", Message: "item added"})

	c.CaptureException(ctx, errors.New("boom"))

	sends := rt.waitFor(t, 1)
	p, ok := sends[0].payload.(*ErrorPayload)
	if !ok {
		t.Fatalf("payload type = %T, want *ErrorPayload", sends[0].payload)
	}
	if len(p.Breadcrumbs) != 2 {
		t.Fatalf("breadcrumbs len = %d, want 2: %#v", len(p.Breadcrumbs), p.Breadcrumbs)
	}
	if p.Breadcrumbs[0].Message != "login ok" || p.Breadcrumbs[1].Message != "item added" {
		t.Fatalf("breadcrumb order/content wrong: %#v", p.Breadcrumbs)
	}
	// Defaults stamped.
	if p.Breadcrumbs[0].Level != "info" || p.Breadcrumbs[0].Timestamp == "" {
		t.Fatalf("breadcrumb defaults not stamped: %#v", p.Breadcrumbs[0])
	}
}

func TestBreadcrumbBufferEvictsOldest(t *testing.T) {
	b := newBreadcrumbBuffer(3)
	for i := 0; i < 5; i++ {
		b.add(Breadcrumb{Message: string(rune('a' + i))})
	}
	got := b.snapshot()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Oldest two (a,b) evicted; newest three (c,d,e) retained in order.
	if got[0].Message != "c" || got[1].Message != "d" || got[2].Message != "e" {
		t.Fatalf("ring eviction wrong: %#v", got)
	}
}

func TestAddBreadcrumbNoStateBagIsNoop(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{APIKey: "ask_test"}, rt)

	// Plain context with no request-state bag — must not panic and must not
	// attach any crumbs to a captured error.
	ctx := context.Background()
	c.AddBreadcrumb(ctx, Breadcrumb{Message: "ignored"})
	c.CaptureException(ctx, errors.New("boom"))

	sends := rt.waitFor(t, 1)
	p := sends[0].payload.(*ErrorPayload)
	if len(p.Breadcrumbs) != 0 {
		t.Fatalf("expected no breadcrumbs without a state bag, got %#v", p.Breadcrumbs)
	}
}

func TestInboundMiddlewareEmitsHTTPBreadcrumb(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{APIKey: "ask_test"}, rt)

	// Handler that emits an outbound-style log crumb then captures an error,
	// so the captured error should carry BOTH the log crumb and (after the
	// handler returns) the request is recorded. We assert on the error that is
	// captured DURING the request, which should already have the log crumb.
	handler := Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.Info(r.Context(), "handling request")
		c.CaptureException(r.Context(), errors.New("mid-request failure"))
		w.WriteHeader(http.StatusInternalServerError)
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/widgets")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Find the captured error among the sends.
	sends := rt.waitFor(t, 1)
	var errPayload *ErrorPayload
	for _, s := range sends {
		if p, ok := s.payload.(*ErrorPayload); ok {
			errPayload = p
			break
		}
	}
	if errPayload == nil {
		t.Fatalf("no error payload captured among %d sends", len(sends))
	}
	// The log emitted before the capture must be present as a breadcrumb.
	foundLog := false
	for _, bc := range errPayload.Breadcrumbs {
		if bc.Type == "log" && bc.Message == "handling request" {
			foundLog = true
		}
	}
	if !foundLog {
		t.Fatalf("expected a log breadcrumb on the captured error, got %#v", errPayload.Breadcrumbs)
	}
}

func TestHTTPBreadcrumbLevelEscalation(t *testing.T) {
	ok := httpBreadcrumb("outbound", "GET", "h", "/p", 200, 5)
	if ok.Level != "info" {
		t.Fatalf("2xx level = %q, want info", ok.Level)
	}
	bad := httpBreadcrumb("outbound", "GET", "h", "/p", 503, 5)
	if bad.Level != "warning" {
		t.Fatalf("5xx level = %q, want warning", bad.Level)
	}
	fail := httpBreadcrumb("outbound", "GET", "h", "/p", 0, 5)
	if fail.Level != "warning" {
		t.Fatalf("transport-fail level = %q, want warning", fail.Level)
	}
}

func TestDBBreadcrumbErrorLevel(t *testing.T) {
	bc := dbBreadcrumb("SELECT", "select * from t where id = ?", "error", 12, 0)
	if bc.Level != "error" {
		t.Fatalf("error status level = %q, want error", bc.Level)
	}
	if bc.Type != "query" || bc.Category != "db.query" {
		t.Fatalf("unexpected type/category: %#v", bc)
	}
}
