package allstakslog

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	allstak "github.com/AllStak/allstak-go"
)

type recorder struct {
	mu    sync.Mutex
	paths []string
	logs  []allstak.LogPayload
	errs  []allstak.ErrorPayload
}

func (r *recorder) send(_ context.Context, path string, payload any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
	switch p := payload.(type) {
	case *allstak.LogPayload:
		r.logs = append(r.logs, *p)
	case *allstak.ErrorPayload:
		r.errs = append(r.errs, *p)
	}
	return nil
}

func (r *recorder) waitPaths(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := len(r.paths)
		r.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d sends", n)
}

func newClient(rec *recorder) *allstak.Client {
	return allstak.NewWithTransport(allstak.Config{
		APIKey:                    "ask_test",
		EnableAutoSessionTracking: ptrFalse(),
	}, allstak.TransportFunc(rec.send))
}

func ptrFalse() *bool { b := false; return &b }

func TestSlogShipsInfoLog(t *testing.T) {
	rec := &recorder{}
	c := newClient(rec)
	defer c.Close(context.Background())

	logger := slog.New(NewHandler(c, &Options{DisableInner: true}))
	logger.Info("hello", "k", "v")

	rec.waitPaths(t, 1)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(rec.logs))
	}
	if rec.logs[0].Level != "info" || rec.logs[0].Message != "hello" {
		t.Fatalf("unexpected log: %#v", rec.logs[0])
	}
	if rec.logs[0].Metadata["k"] != "v" {
		t.Fatalf("attr not carried: %#v", rec.logs[0].Metadata)
	}
}

func TestSlogPromotesErrorWithErrAttr(t *testing.T) {
	rec := &recorder{}
	c := newClient(rec)
	defer c.Close(context.Background())

	logger := slog.New(NewHandler(c, &Options{DisableInner: true}))
	logger.Error("charge failed", slog.Any("err", errors.New("db down")), slog.String("orderId", "o1"))

	rec.waitPaths(t, 2)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(rec.logs))
	}
	if len(rec.errs) != 1 {
		t.Fatalf("errs = %d, want 1 (error should be promoted)", len(rec.errs))
	}
	if rec.errs[0].Message != "db down" {
		t.Fatalf("promoted error message = %q", rec.errs[0].Message)
	}
}

func TestSlogStampsTraceFromContext(t *testing.T) {
	rec := &recorder{}
	c := newClient(rec)
	defer c.Close(context.Background())

	logger := slog.New(NewHandler(c, &Options{DisableInner: true}))
	ctx := allstak.WithContextSpan(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "")
	ctx = allstak.WithRequestID(ctx, "req-123")

	logger.InfoContext(ctx, "with-trace")

	rec.waitPaths(t, 1)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.logs[0].TraceID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("traceId = %q", rec.logs[0].TraceID)
	}
	if rec.logs[0].SpanID != "bbbbbbbbbbbbbbbb" {
		t.Fatalf("spanId = %q", rec.logs[0].SpanID)
	}
	if rec.logs[0].RequestID != "req-123" {
		t.Fatalf("requestId = %q", rec.logs[0].RequestID)
	}
}

func TestSlogWithAttrsAndGroup(t *testing.T) {
	rec := &recorder{}
	c := newClient(rec)
	defer c.Close(context.Background())

	logger := slog.New(NewHandler(c, &Options{DisableInner: true})).
		With("service", "billing").
		WithGroup("http")
	logger.Info("req", "method", "GET")

	rec.waitPaths(t, 1)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	md := rec.logs[0].Metadata
	if md["service"] != "billing" {
		t.Fatalf("WithAttrs not applied: %#v", md)
	}
	if md["http.method"] != "GET" {
		t.Fatalf("WithGroup prefix not applied: %#v", md)
	}
}
