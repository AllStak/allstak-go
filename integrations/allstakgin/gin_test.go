package allstakgin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	allstak "github.com/AllStak/allstak-go"
	"github.com/gin-gonic/gin"
)

func TestMiddlewareCapturesRequestCorrelation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var batches []allstak.HTTPRequestBatch
	client := allstak.NewWithTransport(allstak.Config{
		APIKey:        "ask_test",
		FlushInterval: time.Millisecond,
		BatchSize:     1,
	}, allstak.TransportFunc(func(_ context.Context, path string, payload any) error {
		if path == "/ingest/v1/http-requests" {
			batches = append(batches, payload.(allstak.HTTPRequestBatch))
		}
		return nil
	}))
	defer client.Close(context.Background())

	router := gin.New()
	router.Use(Middleware(client))
	router.GET("/users/:id", func(c *gin.Context) {
		c.String(http.StatusAccepted, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	req.Header.Set("traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01")
	req.Header.Set("X-Request-Id", "req-1")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if rec.Header().Get("X-AllStak-Trace-Id") != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("trace response header missing: %q", rec.Header().Get("X-AllStak-Trace-Id"))
	}
	if rec.Header().Get("X-AllStak-Request-Id") != "req-1" {
		t.Fatalf("request response header missing: %q", rec.Header().Get("X-AllStak-Request-Id"))
	}
	if len(batches) != 1 || len(batches[0].Requests) != 1 {
		t.Fatalf("expected one captured request, got %#v", batches)
	}
	item := batches[0].Requests[0]
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
		t.Fatalf("route path mismatch: %s", item.Path)
	}
	if item.StatusCode != http.StatusAccepted {
		t.Fatalf("status mismatch: %d", item.StatusCode)
	}
}
