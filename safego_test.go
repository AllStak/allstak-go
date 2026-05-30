package allstak

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSafeGoCapturesAndRepanics(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{APIKey: "ask_test"}, rt)

	repanicked := make(chan any, 1)
	// Wrap in our own goroutine that recovers the re-panic, so the test process
	// is not torn down but we still prove SafeGo re-panicked.
	go func() {
		defer func() { repanicked <- recover() }()
		// SafeGo spawns ITS OWN goroutine, so to observe the re-panic we run the
		// guarded body inline via the same guard SafeGo uses.
		func() {
			defer c.recoverAndRepanic(context.Background())
			panic("worker boom")
		}()
	}()

	select {
	case r := <-repanicked:
		if r == nil {
			t.Fatal("expected re-panic, got none")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for re-panic")
	}

	// The panic should have been captured as a fatal error.
	sends := rt.waitFor(t, 1)
	p, ok := sends[0].payload.(*ErrorPayload)
	if !ok {
		t.Fatalf("payload type = %T, want *ErrorPayload", sends[0].payload)
	}
	if p.Level != "fatal" {
		t.Fatalf("level = %q, want fatal", p.Level)
	}
	if p.Message != "worker boom" {
		t.Fatalf("message = %q, want 'worker boom'", p.Message)
	}
}

func TestSafeGoSuppressDoesNotRepanic(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{APIKey: "ask_test"}, rt)

	done := make(chan struct{})
	c.SafeGoSuppress(context.Background(), func() {
		defer close(done)
		panic("suppressed boom")
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not complete")
	}

	sends := rt.waitFor(t, 1)
	p := sends[0].payload.(*ErrorPayload)
	if p.Level != "fatal" || p.Message != "suppressed boom" {
		t.Fatalf("unexpected captured payload: %#v", p)
	}
}

func TestRecoverHandlerCapturesAndReturns500(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{APIKey: "ask_test"}, rt)

	h := c.RecoverHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("handler boom")
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}

	sends := rt.waitFor(t, 1)
	p := sends[0].payload.(*ErrorPayload)
	if p.Level != "fatal" || p.Message != "handler boom" {
		t.Fatalf("unexpected captured payload: %#v", p)
	}
}

func TestGoNilFuncIsSafe(t *testing.T) {
	// Must not panic.
	Go(nil)
	GoCtx(context.Background(), nil)
}
