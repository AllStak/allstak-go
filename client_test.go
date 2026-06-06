package allstak

import (
	"context"
	"sync"
	"testing"
	"time"
)

type recordedSend struct {
	path    string
	payload any
}

type recordingTransport struct {
	mu    sync.Mutex
	sends []recordedSend
}

func (r *recordingTransport) send(_ context.Context, path string, payload any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, recordedSend{path: path, payload: payload})
	return nil
}

func (r *recordingTransport) waitFor(t *testing.T, count int) []recordedSend {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		if len(r.sends) >= count {
			out := append([]recordedSend(nil), r.sends...)
			r.mu.Unlock()
			return out
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Fatalf("recorded %d sends, want at least %d", len(r.sends), count)
	return nil
}

func TestCaptureErrorStampsDefaultsAndReleaseTags(t *testing.T) {
	rt := &recordingTransport{}
	client := newWithTransport(Config{
		Environment:   "staging",
		Release:       "2026.05.22",
		ServiceName:   "orders",
		FlushInterval: time.Hour,
		QueueCapacity: 10,
		BatchSize:     10,
		SDKName:       "allstak-go",
		SDKVersion:    "9.9.9",
		Platform:      "go",
	}, INGEST_HOST, rt)
	defer client.Close(context.Background())

	client.CaptureError(ErrorPayload{
		ExceptionClass: "boom",
		Message:        "failed",
		Metadata:       map[string]any{"sdk.version": "caller-wins"},
	})

	sends := rt.waitFor(t, 1)
	if sends[0].path != pathErrors {
		t.Fatalf("path = %q, want %q", sends[0].path, pathErrors)
	}
	payload, ok := sends[0].payload.(*ErrorPayload)
	if !ok {
		t.Fatalf("payload type = %T, want *ErrorPayload", sends[0].payload)
	}
	if payload.Environment != "staging" {
		t.Fatalf("Environment = %q, want staging", payload.Environment)
	}
	if payload.Release != "2026.05.22" {
		t.Fatalf("Release = %q, want 2026.05.22", payload.Release)
	}
	if payload.Metadata["sdk.name"] != "allstak-go" {
		t.Fatalf("sdk.name = %v, want allstak-go", payload.Metadata["sdk.name"])
	}
	if payload.Metadata["sdk.version"] != "caller-wins" {
		t.Fatalf("caller metadata should win, got %v", payload.Metadata["sdk.version"])
	}
}

func TestCaptureExceptionPromotesRequestCorrelationAndMechanism(t *testing.T) {
	rt := &recordingTransport{}
	client := newWithTransport(Config{
		Environment:   "dev-sdk-audit",
		Release:       "go-test",
		FlushInterval: time.Hour,
		QueueCapacity: 10,
		BatchSize:     10,
	}, INGEST_HOST, rt)
	defer client.Close(context.Background())

	ctx := WithContextSpan(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc")
	ctx = WithRequestID(ctx, "req-go-1")

	client.CaptureException(ctx, errTest("correlated"))

	sends := rt.waitFor(t, 1)
	payload, ok := sends[0].payload.(*ErrorPayload)
	if !ok {
		t.Fatalf("payload type = %T, want *ErrorPayload", sends[0].payload)
	}
	if payload.TraceID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("traceId = %q", payload.TraceID)
	}
	if payload.SpanID != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("spanId = %q", payload.SpanID)
	}
	if payload.ParentSpanID != "cccccccccccccccc" {
		t.Fatalf("parentSpanId = %q", payload.ParentSpanID)
	}
	if payload.RequestID != "req-go-1" {
		t.Fatalf("requestId = %q", payload.RequestID)
	}
	if payload.Mechanism != "captureException" {
		t.Fatalf("mechanism = %q", payload.Mechanism)
	}
	if payload.Handled == nil || !*payload.Handled {
		t.Fatalf("handled = %v, want true", payload.Handled)
	}
}

func TestCaptureLogAndHeartbeatUseExpectedIngestPaths(t *testing.T) {
	rt := &recordingTransport{}
	client := newWithTransport(Config{
		Environment:   "production",
		ServiceName:   "api",
		FlushInterval: time.Hour,
		QueueCapacity: 10,
		BatchSize:     10,
	}, INGEST_HOST, rt)
	defer client.Close(context.Background())

	client.CaptureLog(LogPayload{Level: "info", Message: "hello"})
	if err := client.SendHeartbeat(context.Background(), HeartbeatPayload{Slug: "daily-job"}); err != nil {
		t.Fatalf("SendHeartbeat returned error: %v", err)
	}

	sends := rt.waitFor(t, 2)
	seen := map[string]bool{}
	for _, send := range sends {
		seen[send.path] = true
		if logPayload, ok := send.payload.(*LogPayload); ok {
			if logPayload.Service != "api" {
				t.Fatalf("log service = %q, want api", logPayload.Service)
			}
			if logPayload.Environment != "production" {
				t.Fatalf("log environment = %q, want production", logPayload.Environment)
			}
		}
		if heartbeat, ok := send.payload.(HeartbeatPayload); ok && heartbeat.Status != "success" {
			t.Fatalf("heartbeat status = %q, want success", heartbeat.Status)
		}
	}
	if !seen[pathLogs] {
		t.Fatalf("missing log send to %s", pathLogs)
	}
	if !seen[pathHeartbeat] {
		t.Fatalf("missing heartbeat send to %s", pathHeartbeat)
	}
}
