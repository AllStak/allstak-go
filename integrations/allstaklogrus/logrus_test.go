package allstaklogrus

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	allstak "github.com/AllStak/allstak-go"
	"github.com/sirupsen/logrus"
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

func newLogger(c *allstak.Client) *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard) // keep test output clean; the hook still fires
	l.SetLevel(logrus.DebugLevel)
	l.AddHook(NewHook(c, nil))
	return l
}

func TestLogrusShipsLog(t *testing.T) {
	rec := &recorder{}
	c := newClient(rec)
	defer c.Close(context.Background())

	l := newLogger(c)
	l.WithField("k", "v").Info("hello")

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

func TestLogrusPromotesErrorWithError(t *testing.T) {
	rec := &recorder{}
	c := newClient(rec)
	defer c.Close(context.Background())

	l := newLogger(c)
	l.WithError(errors.New("db down")).WithField("orderId", "o1").Error("charge failed")

	rec.waitCount(t, 2)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.errs) != 1 {
		t.Fatalf("errs = %d, want 1 (should be promoted)", len(rec.errs))
	}
	if rec.errs[0].Message != "db down" {
		t.Fatalf("promoted error = %q", rec.errs[0].Message)
	}
}

func TestLogrusStampsTraceFromContext(t *testing.T) {
	rec := &recorder{}
	c := newClient(rec)
	defer c.Close(context.Background())

	l := newLogger(c)
	ctx := allstak.WithContextSpan(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "")
	ctx = allstak.WithRequestID(ctx, "req-7")
	l.WithContext(ctx).Info("with-trace")

	rec.waitCount(t, 1)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.logs[0].TraceID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("traceId = %q", rec.logs[0].TraceID)
	}
	if rec.logs[0].RequestID != "req-7" {
		t.Fatalf("requestId = %q", rec.logs[0].RequestID)
	}
}

func TestLogrusErrorWithoutErrValueNotPromoted(t *testing.T) {
	rec := &recorder{}
	c := newClient(rec)
	defer c.Close(context.Background())

	l := newLogger(c)
	l.Error("noisy error log, no err value")

	rec.waitCount(t, 1)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.errs) != 0 {
		t.Fatalf("did not expect a promoted error: %#v", rec.errs)
	}
}
