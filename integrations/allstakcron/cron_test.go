package allstakcron_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	allstak "github.com/allstak-io/allstak-go"
	"github.com/allstak-io/allstak-go/integrations/allstakcron"
)

type recordingTransport struct {
	mu         sync.Mutex
	heartbeats []allstak.HeartbeatPayload
	spans      []allstak.SpanItem
	errors     []allstak.ErrorPayload
}

func (r *recordingTransport) send(_ context.Context, path string, payload any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch path {
	case "/ingest/v1/heartbeat":
		if hb, ok := payload.(allstak.HeartbeatPayload); ok {
			r.heartbeats = append(r.heartbeats, hb)
		}
	case "/ingest/v1/spans":
		if batch, ok := payload.(allstak.SpanBatch); ok {
			r.spans = append(r.spans, batch.Spans...)
		}
	case "/ingest/v1/errors":
		if p, ok := payload.(*allstak.ErrorPayload); ok {
			r.errors = append(r.errors, *p)
		}
	}
	return nil
}

func newClient(t *testing.T, rt *recordingTransport) *allstak.Client {
	t.Helper()
	c := allstak.NewWithTransport(
		allstak.Config{APIKey: "ask_test", FlushInterval: 10 * time.Millisecond},
		allstak.TransportFunc(rt.send),
	)
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// TestRunJob_SuccessSendsHeartbeatAndSpan covers the happy path: a job
// returning nil yields a success heartbeat plus a cron span.
func TestRunJob_SuccessSendsHeartbeatAndSpan(t *testing.T) {
	rt := &recordingTransport{}
	c := newClient(t, rt)

	called := false
	err := allstakcron.RunJob(context.Background(), c, "nightly-report", func(ctx context.Context) error {
		called = true
		if sc := allstak.SpanFromContext(ctx); sc == nil || sc.TraceID == "" {
			t.Errorf("expected span context inside job, got nil")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunJob returned err: %v", err)
	}
	if !called {
		t.Fatal("job not invoked")
	}
	if err := c.Flush(timedCtx(t, 2*time.Second)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.heartbeats) != 1 {
		t.Fatalf("want 1 heartbeat, got %d", len(rt.heartbeats))
	}
	hb := rt.heartbeats[0]
	if hb.Slug != "nightly-report" {
		t.Errorf("slug = %q", hb.Slug)
	}
	if hb.Status != "success" {
		t.Errorf("status = %q, want success", hb.Status)
	}
	if len(rt.spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(rt.spans))
	}
	if rt.spans[0].Operation != "cron.nightly-report" {
		t.Errorf("span operation = %q", rt.spans[0].Operation)
	}
}

// TestRunJob_ErrorSendsFailedHeartbeatAndError exercises the failure path.
func TestRunJob_ErrorSendsFailedHeartbeatAndError(t *testing.T) {
	rt := &recordingTransport{}
	c := newClient(t, rt)

	jobErr := errors.New("widget pipeline died")
	err := allstakcron.RunJob(context.Background(), c, "pipeline", func(_ context.Context) error {
		return jobErr
	})
	if err == nil || err.Error() != jobErr.Error() {
		t.Fatalf("expected job error to propagate, got %v", err)
	}
	if err := c.Flush(timedCtx(t, 2*time.Second)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.heartbeats) != 1 {
		t.Fatalf("want 1 heartbeat, got %d", len(rt.heartbeats))
	}
	hb := rt.heartbeats[0]
	if hb.Status != "failed" {
		t.Errorf("status = %q, want failed", hb.Status)
	}
	if hb.Message != "widget pipeline died" {
		t.Errorf("message = %q", hb.Message)
	}
	if len(rt.errors) == 0 {
		t.Fatal("expected error payload to be captured")
	}
}

// TestRunJob_PanicSendsFailedHeartbeatAndDoesNotPropagate verifies that
// a panicking job does not take down the scheduler.
func TestRunJob_PanicSendsFailedHeartbeatAndDoesNotPropagate(t *testing.T) {
	rt := &recordingTransport{}
	c := newClient(t, rt)

	// RunJob recovers panics — must not propagate.
	err := allstakcron.RunJob(context.Background(), c, "panicky", func(_ context.Context) error {
		panic("kaboom")
	})
	if err == nil {
		t.Fatal("RunJob should return a non-nil error after a panic")
	}
	if err := c.Flush(timedCtx(t, 2*time.Second)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.heartbeats) != 1 || rt.heartbeats[0].Status != "failed" {
		t.Fatalf("expected one failed heartbeat, got %#v", rt.heartbeats)
	}
	if len(rt.errors) == 0 {
		t.Fatal("expected at least one error capture from the panic")
	}
}

// TestWrap_BindsToCronAddFuncShape verifies the func()-shape used by
// robfig/cron.AddFunc.
func TestWrap_BindsToCronAddFuncShape(t *testing.T) {
	rt := &recordingTransport{}
	c := newClient(t, rt)

	called := false
	fn := allstakcron.Wrap(c, "every-min", func(_ context.Context) error {
		called = true
		return nil
	})
	// Sanity check the signature is the exact cron.AddFunc shape.
	var _ func() = fn
	fn()
	if !called {
		t.Fatal("Wrap returned func did not invoke the underlying job")
	}
	if err := c.Flush(timedCtx(t, 2*time.Second)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.heartbeats) != 1 {
		t.Fatalf("Wrap should produce 1 heartbeat, got %d", len(rt.heartbeats))
	}
}

// TestRunJob_NilClientOrJobNoOp asserts the integration is safe to
// initialize even when AllStak isn't wired up — important for libraries
// that import allstakcron unconditionally.
func TestRunJob_NilClientOrJobNoOp(t *testing.T) {
	if err := allstakcron.RunJob(context.Background(), nil, "s", func(_ context.Context) error { return nil }); err != nil {
		t.Errorf("nil client: want nil err, got %v", err)
	}
	rt := &recordingTransport{}
	c := newClient(t, rt)
	if err := allstakcron.RunJob(context.Background(), c, "s", nil); err != nil {
		t.Errorf("nil job: want nil err, got %v", err)
	}
}

func timedCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
