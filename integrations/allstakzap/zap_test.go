package allstakzap

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	allstak "github.com/AllStak/allstak-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type recorder struct {
	mu    sync.Mutex
	logs  []allstak.LogPayload
	errs  []allstak.ErrorPayload
	count int
}

func (r *recorder) send(_ context.Context, _ string, payload any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count++
	switch p := payload.(type) {
	case *allstak.LogPayload:
		r.logs = append(r.logs, *p)
	case *allstak.ErrorPayload:
		r.errs = append(r.errs, *p)
	}
	return nil
}

func (r *recorder) waitCount(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := r.count
		r.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d sends", n)
}

func ptrFalse() *bool { b := false; return &b }

func newClient(rec *recorder) *allstak.Client {
	return allstak.NewWithTransport(allstak.Config{
		APIKey:                    "ask_test",
		EnableAutoSessionTracking: ptrFalse(),
	}, allstak.TransportFunc(rec.send))
}

func TestZapShipsLog(t *testing.T) {
	rec := &recorder{}
	c := newClient(rec)
	defer c.Close(context.Background())

	logger := zap.New(NewCore(c, nil))
	logger.Info("hello", zap.String("k", "v"))

	rec.waitCount(t, 1)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(rec.logs))
	}
	if rec.logs[0].Message != "hello" || rec.logs[0].Level != "info" {
		t.Fatalf("unexpected log: %#v", rec.logs[0])
	}
	if rec.logs[0].Metadata["k"] != "v" {
		t.Fatalf("field not carried: %#v", rec.logs[0].Metadata)
	}
}

func TestZapPromotesErrorWithError(t *testing.T) {
	rec := &recorder{}
	c := newClient(rec)
	defer c.Close(context.Background())

	logger := zap.New(NewCore(c, nil))
	logger.Error("charge failed", zap.Error(errors.New("db down")), zap.String("orderId", "o1"))

	rec.waitCount(t, 2)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(rec.logs))
	}
	if len(rec.errs) != 1 {
		t.Fatalf("errs = %d, want 1 (should be promoted)", len(rec.errs))
	}
	if rec.errs[0].Message != "db down" {
		t.Fatalf("promoted error = %q", rec.errs[0].Message)
	}
}

func TestZapWithContextStampsTrace(t *testing.T) {
	rec := &recorder{}
	c := newClient(rec)
	defer c.Close(context.Background())

	logger := zap.New(NewCore(c, nil))
	ctx := allstak.WithContextSpan(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "")
	logger.Info("with-trace", WithContext(ctx), zap.String("k", "v"))

	rec.waitCount(t, 1)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.logs[0].TraceID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("traceId = %q", rec.logs[0].TraceID)
	}
	if rec.logs[0].SpanID != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("spanId = %q", rec.logs[0].SpanID)
	}
	// The smuggled context field must NOT leak into the log metadata.
	if _, ok := rec.logs[0].Metadata[ctxFieldKey]; ok {
		t.Fatalf("context field leaked into metadata: %#v", rec.logs[0].Metadata)
	}
}

func TestZapWrapTees(t *testing.T) {
	rec := &recorder{}
	c := newClient(rec)
	defer c.Close(context.Background())

	// A base logger with a no-op core; Wrap tees an AllStak core onto it.
	base := zap.New(zapcore.NewNopCore())
	logger := Wrap(base, c, nil)
	logger.Warn("teed")

	rec.waitCount(t, 1)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.logs[0].Level != "warn" {
		t.Fatalf("level = %q, want warn", rec.logs[0].Level)
	}
}

func TestZapBelowMinLevelDropped(t *testing.T) {
	rec := &recorder{}
	c := newClient(rec)
	defer c.Close(context.Background())

	logger := zap.New(NewCore(c, &Options{MinLevel: zapcore.WarnLevel}))
	logger.Info("should be dropped")
	logger.Warn("should ship")

	rec.waitCount(t, 1)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, l := range rec.logs {
		if l.Message == "should be dropped" {
			t.Fatalf("info below min level should not have shipped")
		}
	}
}
