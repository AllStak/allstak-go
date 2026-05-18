package allstak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCaptureErrorReturnsFastWhenTransportIsSlow(t *testing.T) {
	client := NewWithTransport(Config{
		APIKey:         "ask_test",
		QueueCapacity:  4,
		FlushInterval:  time.Hour,
		RequestTimeout: 50 * time.Millisecond,
	}, TransportFunc(func(ctx context.Context, path string, payload any) error {
		select {
		case <-time.After(500 * time.Millisecond):
			return errors.New("ingest unavailable")
		case <-ctx.Done():
			return ctx.Err()
		}
	}))

	start := time.Now()
	client.CaptureError(ErrorPayload{ExceptionClass: "FailOpen", Message: "boom"})
	if elapsed := time.Since(start); elapsed > 25*time.Millisecond {
		t.Fatalf("CaptureError blocked on transport: %s", elapsed)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = client.Close(closeCtx)
}

func TestQueueOverflowDropsWithoutBlocking(t *testing.T) {
	client := NewWithTransport(Config{
		APIKey:         "ask_test",
		QueueCapacity:  1,
		FlushInterval:  time.Hour,
		RequestTimeout: 25 * time.Millisecond,
	}, TransportFunc(func(ctx context.Context, path string, payload any) error {
		<-ctx.Done()
		return ctx.Err()
	}))

	start := time.Now()
	for i := 0; i < 100; i++ {
		client.CaptureError(ErrorPayload{ExceptionClass: "Overflow", Message: "drop safely"})
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("queue overflow blocked request path: %s", elapsed)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = client.Close(closeCtx)
}

func TestHeartbeatIsBoundedByCallerContext(t *testing.T) {
	client := NewWithTransport(Config{
		APIKey:         "ask_test",
		RequestTimeout: time.Second,
	}, TransportFunc(func(ctx context.Context, path string, payload any) error {
		<-ctx.Done()
		return ctx.Err()
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := client.SendHeartbeat(ctx, HeartbeatPayload{Slug: "fail-open", Status: "success"})
	if err == nil {
		t.Fatal("expected context error from bounded heartbeat")
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("heartbeat exceeded caller context bound: %s", elapsed)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer closeCancel()
	_ = client.Close(closeCtx)
}

func TestHeartbeatWithoutDeadlineGetsInternalBound(t *testing.T) {
	client := NewWithTransport(Config{
		APIKey:         "ask_test",
		RequestTimeout: 40 * time.Millisecond,
	}, TransportFunc(func(ctx context.Context, path string, payload any) error {
		<-ctx.Done()
		return ctx.Err()
	}))

	start := time.Now()
	err := client.SendHeartbeat(context.Background(), HeartbeatPayload{Slug: "fail-open", Status: "success"})
	if err == nil {
		t.Fatal("expected context error from internal heartbeat bound")
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("heartbeat exceeded internal bound: %s", elapsed)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer closeCancel()
	_ = client.Close(closeCtx)
}

func TestFlushIsBoundedByCallerContext(t *testing.T) {
	client := NewWithTransport(Config{
		APIKey:         "ask_test",
		FlushInterval:  time.Hour,
		RequestTimeout: time.Second,
	}, TransportFunc(func(ctx context.Context, path string, payload any) error {
		<-ctx.Done()
		return ctx.Err()
	}))

	client.CaptureError(ErrorPayload{ExceptionClass: "Flush", Message: "bounded"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_ = client.Flush(ctx)
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("flush exceeded caller context bound: %s", elapsed)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer closeCancel()
	_ = client.Close(closeCtx)
}

func TestHTTPMiddlewareDoesNotWaitOnTelemetryTransport(t *testing.T) {
	client := NewWithTransport(Config{
		APIKey:         "ask_test",
		QueueCapacity:  2,
		FlushInterval:  time.Hour,
		RequestTimeout: 50 * time.Millisecond,
	}, TransportFunc(func(ctx context.Context, path string, payload any) error {
		select {
		case <-time.After(500 * time.Millisecond):
			return errors.New("ingest unavailable")
		case <-ctx.Done():
			return ctx.Err()
		}
	}))

	handler := Middleware(client)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("customer response"))
	}))

	start := time.Now()
	req := httptest.NewRequest(http.MethodGet, "http://customer.test/orders", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("customer response changed: got %d", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("middleware blocked on AllStak transport: %s", elapsed)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer closeCancel()
	_ = client.Close(closeCtx)
}

func TestHTTPMiddlewareExtractsTraceparentAndRequestID(t *testing.T) {
	type capturedPost struct {
		path    string
		payload any
	}
	posts := make(chan capturedPost, 4)
	client := NewWithTransport(Config{
		APIKey:        "ask_test",
		QueueCapacity: 4,
		BatchSize:     1,
		FlushInterval: time.Hour,
	}, TransportFunc(func(ctx context.Context, path string, payload any) error {
		posts <- capturedPost{path: path, payload: payload}
		return nil
	}))

	handler := Middleware(client)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	traceID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	parentSpan := "bbbbbbbbbbbbbbbb"
	requestID := "req-go-fixture"
	req := httptest.NewRequest(http.MethodGet, "http://customer.test/orders", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-"+parentSpan+"-01")
	req.Header.Set("X-AllStak-Request-Id", requestID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-AllStak-Trace-Id"); got != traceID {
		t.Fatalf("response trace id = %q, want %q", got, traceID)
	}
	if got := rec.Header().Get("X-AllStak-Request-Id"); got != requestID {
		t.Fatalf("response request id = %q, want %q", got, requestID)
	}
	if got := rec.Header().Get("traceparent"); got == "" {
		t.Fatal("response traceparent missing")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = client.Flush(ctx)

	select {
	case post := <-posts:
		if post.path != pathHTTPRequests {
			t.Fatalf("post path = %s", post.path)
		}
		var batch HTTPRequestBatch
		body, _ := json.Marshal(post.payload)
		if err := json.Unmarshal(body, &batch); err != nil {
			t.Fatal(err)
		}
		if len(batch.Requests) != 1 {
			t.Fatalf("request batch size = %d", len(batch.Requests))
		}
		if batch.Requests[0].TraceID != traceID {
			t.Fatalf("trace id = %q, want %q", batch.Requests[0].TraceID, traceID)
		}
		if batch.Requests[0].ParentSpanID != parentSpan {
			t.Fatalf("parent span id = %q, want %q", batch.Requests[0].ParentSpanID, parentSpan)
		}
		if batch.Requests[0].Metadata["requestId"] != requestID {
			t.Fatalf("request id metadata = %v, want %q", batch.Requests[0].Metadata["requestId"], requestID)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for request telemetry")
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer closeCancel()
	_ = client.Close(closeCtx)
}

func TestOutboundTransportPropagatesTraceparentAndRequestID(t *testing.T) {
	client := NewWithTransport(Config{
		APIKey:        "ask_test",
		QueueCapacity: 4,
		BatchSize:     1,
		FlushInterval: time.Hour,
	}, TransportFunc(func(ctx context.Context, path string, payload any) error {
		return nil
	}))

	inner := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("X-AllStak-Trace-Id"); got != "cccccccccccccccccccccccccccccccc" {
			t.Fatalf("outbound trace id = %q", got)
		}
		if got := req.Header.Get("X-AllStak-Request-Id"); got != "req-outbound-go" {
			t.Fatalf("outbound request id = %q", got)
		}
		if got := req.Header.Get("traceparent"); got != "00-cccccccccccccccccccccccccccccccc-dddddddddddddddd-01" {
			t.Fatalf("outbound traceparent = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       http.NoBody,
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	ctx := WithRequestID(context.Background(), "req-outbound-go")
	ctx = WithContextSpan(ctx, "cccccccccccccccccccccccccccccccc", "dddddddddddddddd", "")
	req := httptest.NewRequest(http.MethodPost, "https://downstream.example.test/sync", nil).WithContext(ctx)
	resp, err := NewTransport(client, inner).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer closeCancel()
	_ = client.Close(closeCtx)
}

func TestHTTPTransport503And429AreBounded(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"down":true}`))
			}))
			defer server.Close()

			transport := newHTTPTransport(server.URL, "ask_test", 40*time.Millisecond, 1, false)
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := transport.send(ctx, pathErrors, ErrorPayload{ExceptionClass: "FailOpen", Message: "status"})
			if err == nil {
				t.Fatal("expected bounded transport error")
			}
			if elapsed := time.Since(start); elapsed > 180*time.Millisecond {
				t.Fatalf("transport retry path exceeded bound: %s", elapsed)
			}
		})
	}
}

func TestHTTPTransportConnectionFailureIsBounded(t *testing.T) {
	transport := newHTTPTransport("http://127.0.0.1:1", "ask_test", 40*time.Millisecond, 1, false)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := transport.send(ctx, pathErrors, ErrorPayload{ExceptionClass: "FailOpen", Message: "connect"})
	if err == nil {
		t.Fatal("expected connection failure")
	}
	if elapsed := time.Since(start); elapsed > 180*time.Millisecond {
		t.Fatalf("connection failure exceeded bound: %s", elapsed)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
