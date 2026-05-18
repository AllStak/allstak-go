package allstakgin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	allstak "github.com/AllStak/allstak-go"
	"github.com/AllStak/allstak-go/integrations/allstakgin"
	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
	// Point any transport attempt at a guaranteed-unreachable address so a
	// missing backend cannot influence test latency or behavior.
	_ = (func() error { return nil })()
}

// newClient builds a Client whose transport never blocks the test even if no
// AllStak backend is reachable. The Middleware contract is purely fail-open;
// these tests verify that contract holds at the Gin layer.
func newClient(t *testing.T) *allstak.Client {
	t.Helper()
	t.Setenv("ALLSTAK_HOST", "http://127.0.0.1:1") // unreachable on purpose
	c := allstak.New(allstak.Config{APIKey: "ask_test"})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_ = c.Close(ctx)
	})
	return c
}

func TestGinMiddlewareInjectsCorrelationHeadersOnResponse(t *testing.T) {
	c := newClient(t)
	r := gin.New()
	r.Use(allstakgin.Middleware(c))
	r.GET("/ok", func(ctx *gin.Context) { ctx.Status(200) })

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("X-AllStak-Trace-Id"); got == "" || len(got) != 32 {
		t.Fatalf("X-AllStak-Trace-Id missing or wrong length: %q", got)
	}
	if got := w.Header().Get("X-AllStak-Request-Id"); got == "" {
		t.Fatalf("X-AllStak-Request-Id missing")
	}
	tp := w.Header().Get("traceparent")
	if !strings.HasPrefix(tp, "00-") || strings.Count(tp, "-") != 3 {
		t.Fatalf("traceparent malformed: %q", tp)
	}
}

func TestGinMiddlewareHonoursInboundTraceparent(t *testing.T) {
	c := newClient(t)
	r := gin.New()
	r.Use(allstakgin.Middleware(c))
	r.GET("/ok", func(ctx *gin.Context) { ctx.Status(200) })

	const inboundTraceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const inboundParent = "bbbbbbbbbbbbbbbb"
	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set("traceparent", "00-"+inboundTraceID+"-"+inboundParent+"-01")
	req.Header.Set("X-AllStak-Request-Id", "req-from-upstream")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("X-AllStak-Trace-Id"); got != inboundTraceID {
		t.Fatalf("trace id not propagated; want=%q got=%q", inboundTraceID, got)
	}
	if got := w.Header().Get("X-AllStak-Request-Id"); got != "req-from-upstream" {
		t.Fatalf("request id not propagated; got=%q", got)
	}
}

func TestGinMiddlewareDoesNotCrashOnPanic(t *testing.T) {
	c := newClient(t)
	r := gin.New()
	r.Use(allstakgin.Middleware(c))
	r.GET("/boom", func(ctx *gin.Context) { panic("boom") })

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	w := httptest.NewRecorder()

	// Must not propagate the panic — the middleware recovers and writes 500.
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("panic should have been recovered, got %v", rec)
		}
	}()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 after recovered panic, got %d", w.Code)
	}
}
