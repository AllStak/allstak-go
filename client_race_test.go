package allstak

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestCaptureCloseRaceStress hammers every Capture* path from hundreds of
// goroutines while Close() runs concurrently, repeated 100+ times. Before the
// sendMu synchronization a sender could pass the closed.Load() fast path, be
// descheduled past Flush's grace window, and execute its channel send after
// close(chan) — panicking with "send on closed channel" inside the host
// process. This test must pass under `go test -race` with zero panics.
func TestCaptureCloseRaceStress(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		prev := runtime.GOMAXPROCS(4)
		defer runtime.GOMAXPROCS(prev)
	}

	const iterations = 120
	const goroutines = 200
	const capturesPerGoroutine = 5

	for i := 0; i < iterations; i++ {
		client := newWithTransport(Config{
			Environment:   "test",
			Release:       "race-test",
			ServiceName:   "race",
			FlushInterval: 5 * time.Millisecond,
			BatchSize:     16,
			QueueCapacity: 64,
			SDKName:       "allstak-go",
			SDKVersion:    "0.0.0-test",
			Platform:      "go",
		}, INGEST_HOST, TransportFunc(func(context.Context, string, any) error {
			return nil
		}))

		gate := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for g := 0; g < goroutines; g++ {
			go func(g int) {
				defer wg.Done()
				<-gate
				ctx := context.Background()
				for j := 0; j < capturesPerGoroutine; j++ {
					switch (g + j) % 6 {
					case 0:
						client.CaptureException(ctx, errors.New("race-boom"))
					case 1:
						client.CaptureMessage(ctx, "error", "race-message")
					case 2:
						client.CaptureLog(LogPayload{Level: "info", Message: "race-log"})
					case 3:
						client.CaptureHTTPRequest(HTTPRequestItem{Method: "GET", Path: "/race"})
					case 4:
						client.CaptureDBQuery(DBQueryItem{NormalizedQuery: "SELECT ?", Status: "success"})
					case 5:
						client.CaptureSpan(SpanItem{TraceID: "t", SpanID: "s", Operation: "race"})
					}
				}
			}(g)
		}

		// Close races the captures: the gate releases all senders and Close at
		// the same instant, maximizing the window where a sender has passed
		// the closed fast path but not yet sent.
		closeDone := make(chan error, 1)
		go func() {
			<-gate
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			closeDone <- client.Close(ctx)
		}()

		close(gate)
		wg.Wait()
		<-closeDone

		// Late captures after a completed Close must be silent no-ops.
		client.CaptureError(ErrorPayload{ExceptionClass: "late", Message: "after close"})
		client.CaptureLog(LogPayload{Level: "info", Message: "after close"})
	}
}

// TestCaptureAfterCloseIsNoOp pins the post-Close contract: every capture
// path returns without panicking and without enqueueing.
func TestCaptureAfterCloseIsNoOp(t *testing.T) {
	rt := &recordingTransport{}
	client := newWithTransport(Config{
		FlushInterval: time.Hour,
		QueueCapacity: 8,
		BatchSize:     8,
	}, INGEST_HOST, rt)
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	client.CaptureError(ErrorPayload{ExceptionClass: "x", Message: "y"})
	client.CaptureLog(LogPayload{Level: "info", Message: "z"})
	client.CaptureHTTPRequest(HTTPRequestItem{Method: "GET"})
	client.CaptureDBQuery(DBQueryItem{NormalizedQuery: "SELECT 1"})
	client.CaptureSpan(SpanItem{Operation: "op"})
	if err := client.SendHeartbeat(context.Background(), HeartbeatPayload{Slug: "job"}); err == nil {
		t.Fatal("SendHeartbeat after Close should return an error")
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, s := range rt.sends {
		if s.path == pathErrors || s.path == pathLogs {
			t.Fatalf("capture after Close reached the transport: %s", s.path)
		}
	}
}

// TestWorkerSurvivesPanickingTransport proves a panicking transport (or
// persist path) can never take down the worker goroutine — and therefore the
// host process. Events are counted failed+dropped and the client still closes
// cleanly.
func TestWorkerSurvivesPanickingTransport(t *testing.T) {
	off := false
	client := newWithTransport(Config{
		FlushInterval:      5 * time.Millisecond,
		QueueCapacity:      16,
		BatchSize:          4,
		EnableOfflineQueue: &off,
	}, INGEST_HOST, TransportFunc(func(_ context.Context, path string, _ any) error {
		panic(fmt.Sprintf("transport bug on %s", path))
	}))

	client.CaptureError(ErrorPayload{ExceptionClass: "boom", Message: "panic path"})
	client.CaptureLog(LogPayload{Level: "info", Message: "panic path"})
	client.CaptureDBQuery(DBQueryItem{NormalizedQuery: "SELECT 1", Status: "success"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if client.Stats().Failed >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := client.Stats().Failed; got < 2 {
		t.Fatalf("Failed = %d, want >= 2 (panics converted to failures)", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		t.Fatalf("Close after transport panics: %v", err)
	}
}

// TestDebugWriterConcurrentSetAndUse pins F8: constructing a Debug client
// (which installs os.Stderr as the debug writer) while other goroutines write
// debug output must be race-free. Run under -race.
func TestDebugWriterConcurrentSetAndUse(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				setDebugWriter(io.Discard)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				fmt.Fprintf(debugWriter(), "concurrent debug line %d\n", j)
			}
		}()
	}
	wg.Wait()
}

// TestSendHeartbeatConvertsPanicToError pins the public-API-boundary guard.
func TestSendHeartbeatConvertsPanicToError(t *testing.T) {
	client := newWithTransport(Config{
		FlushInterval: time.Hour,
		QueueCapacity: 8,
		BatchSize:     8,
	}, INGEST_HOST, TransportFunc(func(context.Context, string, any) error {
		panic("heartbeat transport bug")
	}))
	defer client.Close(context.Background())

	err := client.SendHeartbeat(context.Background(), HeartbeatPayload{Slug: "job"})
	if err == nil {
		t.Fatal("want error from panicking transport, got nil")
	}
}
