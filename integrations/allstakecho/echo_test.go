package allstakecho

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	allstak "github.com/AllStak/allstak-go"
	"github.com/labstack/echo/v4"
)

// capture collects the ingest payloads the middleware emits so assertions
// can inspect both the inbound HTTP request batch and the error events.
type capture struct {
	mu       sync.Mutex
	requests []allstak.HTTPRequestBatch
	errors   []*allstak.ErrorPayload
}

func (c *capture) transport() allstak.TransportFunc {
	return func(_ context.Context, path string, payload any) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch path {
		case "/ingest/v1/http-requests":
			c.requests = append(c.requests, payload.(allstak.HTTPRequestBatch))
		case "/ingest/v1/errors":
			c.errors = append(c.errors, payload.(*allstak.ErrorPayload))
		}
		return nil
	}
}

func newClient(t *testing.T, cap *capture) *allstak.Client {
	t.Helper()
	return allstak.NewWithTransport(allstak.Config{
		APIKey:        "ask_test",
		FlushInterval: time.Millisecond,
		BatchSize:     1,
	}, cap.transport())
}

func TestMiddlewareCapturesRequestCorrelation(t *testing.T) {
	cap := &capture{}
	client := newClient(t, cap)
	defer client.Close(context.Background())

	e := echo.New()
	e.Use(Middleware(client))
	e.GET("/users/:id", func(c echo.Context) error {
		return c.String(http.StatusAccepted, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	req.Header.Set("traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01")
	req.Header.Set("X-Request-Id", "req-1")
	rec := httptest.NewRecorder()

	e.ServeHTTP(rec, req)
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if rec.Header().Get("X-AllStak-Trace-Id") != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("trace response header missing: %q", rec.Header().Get("X-AllStak-Trace-Id"))
	}
	if rec.Header().Get("X-AllStak-Request-Id") != "req-1" {
		t.Fatalf("request response header missing: %q", rec.Header().Get("X-AllStak-Request-Id"))
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.requests) != 1 || len(cap.requests[0].Requests) != 1 {
		t.Fatalf("expected one captured request, got %#v", cap.requests)
	}
	item := cap.requests[0].Requests[0]
	if item.TraceID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("trace id mismatch: %s", item.TraceID)
	}
	if item.RequestID != "req-1" {
		t.Fatalf("request id mismatch: %s", item.RequestID)
	}
	if item.ParentSpanID != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("parent span mismatch: %s", item.ParentSpanID)
	}
	if item.Path != "/users/:id" {
		t.Fatalf("route path mismatch (want route template, got): %s", item.Path)
	}
	if item.StatusCode != http.StatusAccepted {
		t.Fatalf("status mismatch: %d", item.StatusCode)
	}
}

func TestMiddlewareCapturesPanicAsFatal(t *testing.T) {
	cap := &capture{}
	client := newClient(t, cap)
	defer client.Close(context.Background())

	e := echo.New()
	// Silence Echo's default error logging for the expected panic.
	e.Logger.SetOutput(io.Discard)
	e.Use(Middleware(client))
	e.GET("/boom", func(c echo.Context) error {
		panic("kaboom")
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()

	// Echo's router runs the handler chain inside its own recover, so the
	// re-panic is contained by the framework and surfaces as a 500.
	e.ServeHTTP(rec, req)
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.errors) != 1 {
		t.Fatalf("expected one captured fatal error, got %d: %#v", len(cap.errors), cap.errors)
	}
	if cap.errors[0].Level != "fatal" {
		t.Fatalf("panic should be captured as fatal, got level=%q", cap.errors[0].Level)
	}
	if cap.errors[0].Message != "kaboom" {
		t.Fatalf("panic message mismatch: %q", cap.errors[0].Message)
	}
	// The inbound request must still be recorded even though the handler panicked.
	if len(cap.requests) != 1 || len(cap.requests[0].Requests) != 1 {
		t.Fatalf("expected inbound request recorded on panic, got %#v", cap.requests)
	}
	if got := cap.requests[0].Requests[0].Path; got != "/boom" {
		t.Fatalf("panic-path route mismatch: %s", got)
	}
}

func TestMiddlewareCapturesHandlerError(t *testing.T) {
	cap := &capture{}
	client := newClient(t, cap)
	defer client.Close(context.Background())

	e := echo.New()
	e.Use(Middleware(client))
	e.GET("/fail", func(c echo.Context) error {
		return errors.New("handler blew up")
	})

	req := httptest.NewRequest(http.MethodGet, "/fail", nil)
	rec := httptest.NewRecorder()

	e.ServeHTTP(rec, req)
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.errors) != 1 {
		t.Fatalf("expected one captured error, got %d: %#v", len(cap.errors), cap.errors)
	}
	if cap.errors[0].Level != "error" {
		t.Fatalf("handler error should be captured as error, got level=%q", cap.errors[0].Level)
	}
	if cap.errors[0].Message != "handler blew up" {
		t.Fatalf("error message mismatch: %q", cap.errors[0].Message)
	}
	// Echo's default error handler turns the returned error into a 500, and
	// the inbound request is still recorded with the matched route template.
	if len(cap.requests) != 1 || len(cap.requests[0].Requests) != 1 {
		t.Fatalf("expected inbound request recorded, got %#v", cap.requests)
	}
	if got := cap.requests[0].Requests[0].Path; got != "/fail" {
		t.Fatalf("route path mismatch: %s", got)
	}
}
