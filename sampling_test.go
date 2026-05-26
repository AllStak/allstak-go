package allstak

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// withSampler swaps the package-level sampler seam for the duration of a test
// and restores it afterward. The returned value comes from a deterministic
// closure so tests can drive shouldSampleError/shouldSampleTrace decisions
// without relying on real randomness.
func withSampler(t *testing.T, draw func() float64) {
	t.Helper()
	sampleMu.Lock()
	prev := sampleFunc
	sampleFunc = draw
	sampleMu.Unlock()
	t.Cleanup(func() {
		sampleMu.Lock()
		sampleFunc = prev
		sampleMu.Unlock()
	})
}

func newTestClient(t *testing.T, cfg Config, rt *recordingTransport) *Client {
	t.Helper()
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = time.Hour
	}
	if cfg.QueueCapacity == 0 {
		cfg.QueueCapacity = 16
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 16
	}
	c := newWithTransport(cfg.applyDefaults(), INGEST_HOST, rt)
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// ── BeforeSend ─────────────────────────────────────────────────────────────

func TestBeforeSendMutatesEvent(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{
		BeforeSend: func(e *ErrorPayload) *ErrorPayload {
			e.Message = "rewritten"
			if e.Metadata == nil {
				e.Metadata = map[string]any{}
			}
			e.Metadata["scrubbed_by_before_send"] = true
			return e
		},
	}, rt)

	c.CaptureMessage(context.Background(), "info", "original")

	sends := rt.waitFor(t, 1)
	p, ok := sends[0].payload.(*ErrorPayload)
	if !ok {
		t.Fatalf("payload type = %T, want *ErrorPayload", sends[0].payload)
	}
	if p.Message != "rewritten" {
		t.Fatalf("Message = %q, want rewritten (BeforeSend mutation not applied)", p.Message)
	}
	if p.Metadata["scrubbed_by_before_send"] != true {
		t.Fatalf("BeforeSend metadata mutation not applied: %v", p.Metadata)
	}
}

func TestBeforeSendNilDropsEvent(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{
		BeforeSend: func(e *ErrorPayload) *ErrorPayload { return nil },
	}, rt)

	c.CaptureMessage(context.Background(), "info", "should be dropped")
	c.CaptureException(context.Background(), errTest("boom"))

	// Give the worker a beat — nothing should arrive.
	time.Sleep(60 * time.Millisecond)
	rt.mu.Lock()
	got := len(rt.sends)
	rt.mu.Unlock()
	if got != 0 {
		t.Fatalf("recorded %d sends, want 0 (BeforeSend returning nil must drop)", got)
	}
	if d := c.Stats().Dropped; d != 2 {
		t.Fatalf("Dropped = %d, want 2", d)
	}
}

func TestBeforeSendPanicFailsOpen(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{
		BeforeSend: func(e *ErrorPayload) *ErrorPayload {
			panic("user callback bug")
		},
	}, rt)

	// Must not panic the caller.
	c.CaptureMessage(context.Background(), "info", "survive the panic")

	sends := rt.waitFor(t, 1)
	p, ok := sends[0].payload.(*ErrorPayload)
	if !ok {
		t.Fatalf("payload type = %T, want *ErrorPayload", sends[0].payload)
	}
	if p.Message != "survive the panic" {
		t.Fatalf("Message = %q, want original (fail-open should send unmodified event)", p.Message)
	}
}

// ── SampleRate ─────────────────────────────────────────────────────────────

func TestSampleRateZeroKeepsAll(t *testing.T) {
	rt := &recordingTransport{}
	// SampleRate unset (0) must mean keep-all. Sampler should not even be
	// consulted, but stub it to a value that WOULD drop to prove it isn't.
	withSampler(t, func() float64 { return 0.99 })
	c := newTestClient(t, Config{SampleRate: 0}, rt)

	c.CaptureMessage(context.Background(), "info", "kept")

	sends := rt.waitFor(t, 1)
	if len(sends) != 1 {
		t.Fatalf("recorded %d sends, want 1", len(sends))
	}
}

func TestSampleRateFullKeepsAll(t *testing.T) {
	rt := &recordingTransport{}
	withSampler(t, func() float64 { return 0.999999 })
	c := newTestClient(t, Config{SampleRate: 1.0}, rt)

	c.CaptureMessage(context.Background(), "info", "kept")

	if len(rt.waitFor(t, 1)) != 1 {
		t.Fatalf("SampleRate 1.0 must keep the event")
	}
}

func TestSampleRatePartialDropsBelowThreshold(t *testing.T) {
	rt := &recordingTransport{}
	// rate = 0.5; draw 0.9 → 0.9 < 0.5 is false → DROP.
	withSampler(t, func() float64 { return 0.9 })
	c := newTestClient(t, Config{SampleRate: 0.5}, rt)

	c.CaptureMessage(context.Background(), "info", "dropped")

	time.Sleep(60 * time.Millisecond)
	rt.mu.Lock()
	got := len(rt.sends)
	rt.mu.Unlock()
	if got != 0 {
		t.Fatalf("recorded %d sends, want 0 (draw above rate must drop)", got)
	}
	if d := c.Stats().Dropped; d != 1 {
		t.Fatalf("Dropped = %d, want 1", d)
	}
}

func TestSampleRatePartialKeepsBelowThreshold(t *testing.T) {
	rt := &recordingTransport{}
	// rate = 0.5; draw 0.1 → 0.1 < 0.5 is true → KEEP.
	withSampler(t, func() float64 { return 0.1 })
	c := newTestClient(t, Config{SampleRate: 0.5}, rt)

	c.CaptureMessage(context.Background(), "info", "kept")

	if len(rt.waitFor(t, 1)) != 1 {
		t.Fatalf("draw below rate must keep the event")
	}
}

// SampleRate must compose with BeforeSend: a dropped sample never reaches
// BeforeSend.
func TestSampleRateDropSkipsBeforeSend(t *testing.T) {
	rt := &recordingTransport{}
	var beforeSendCalled bool
	withSampler(t, func() float64 { return 0.9 }) // drops at rate 0.5
	c := newTestClient(t, Config{
		SampleRate: 0.5,
		BeforeSend: func(e *ErrorPayload) *ErrorPayload {
			beforeSendCalled = true
			return e
		},
	}, rt)

	c.CaptureMessage(context.Background(), "info", "dropped before BeforeSend")

	time.Sleep(60 * time.Millisecond)
	if beforeSendCalled {
		t.Fatalf("BeforeSend ran for a sampled-out event; SampleRate must gate first")
	}
}

// ── TracesSampleRate / traceparent flag ────────────────────────────────────

func TestTracesSampleRateRecordsAndPropagatesSampled(t *testing.T) {
	rt := &recordingTransport{}
	withSampler(t, func() float64 { return 0.0 }) // 0.0 < 0.5 → sampled
	// Short flush interval so the batched span worker drains promptly.
	c := newTestClient(t, Config{TracesSampleRate: 0.5, FlushInterval: 20 * time.Millisecond}, rt)

	ctx, finish := c.StartSpan(context.Background(), "checkout")
	// Trace context should be marked sampled.
	sc := SpanFromContext(ctx)
	if sc == nil || !sc.Sampled {
		t.Fatalf("span context Sampled = %v, want true", sc)
	}
	// Outbound propagation should emit -01.
	req := httptest.NewRequest("GET", "http://downstream/", nil).WithContext(ctx)
	SetTraceRequestHeaders(req, sc.TraceID, "", sc.SpanID)
	if got := req.Header.Get("traceparent"); !endsWith(got, "-01") {
		t.Fatalf("traceparent = %q, want sampled (-01)", got)
	}
	finish(nil)

	// Spans are batched; force a flush so the worker drains before we assert.
	_ = c.Flush(context.Background())
	if len(rt.waitFor(t, 1)) != 1 {
		t.Fatalf("sampled span must be recorded")
	}
}

func TestTracesSampleRateDropsAndPropagatesNotSampled(t *testing.T) {
	rt := &recordingTransport{}
	withSampler(t, func() float64 { return 0.9 }) // 0.9 < 0.5 false → not sampled
	c := newTestClient(t, Config{TracesSampleRate: 0.5}, rt)

	ctx, finish := c.StartSpan(context.Background(), "checkout")
	sc := SpanFromContext(ctx)
	if sc == nil || sc.Sampled {
		t.Fatalf("span context Sampled = %v, want false", sc)
	}
	// Outbound propagation should emit -00.
	req := httptest.NewRequest("GET", "http://downstream/", nil).WithContext(ctx)
	SetTraceRequestHeaders(req, sc.TraceID, "", sc.SpanID)
	if got := req.Header.Get("traceparent"); !endsWith(got, "-00") {
		t.Fatalf("traceparent = %q, want not-sampled (-00)", got)
	}
	finish(nil)

	// Not-sampled span must NOT be recorded. Flush forces the batch worker to
	// drain anything queued so a 0 count is meaningful, not just untimed.
	_ = c.Flush(context.Background())
	time.Sleep(40 * time.Millisecond)
	rt.mu.Lock()
	got := len(rt.sends)
	rt.mu.Unlock()
	if got != 0 {
		t.Fatalf("recorded %d span sends, want 0 (not-sampled span must be skipped)", got)
	}
}

func TestTracesSampleRateZeroRecordsAll(t *testing.T) {
	rt := &recordingTransport{}
	// Unset TracesSampleRate (0) → record everything; sampler stubbed to drop
	// to prove it isn't consulted.
	withSampler(t, func() float64 { return 0.999 })
	c := newTestClient(t, Config{TracesSampleRate: 0, FlushInterval: 20 * time.Millisecond}, rt)

	_, finish := c.StartSpan(context.Background(), "always")
	finish(nil)

	_ = c.Flush(context.Background())
	if len(rt.waitFor(t, 1)) != 1 {
		t.Fatalf("TracesSampleRate 0 (disabled) must record the span")
	}
}

func TestStartSpanChildInheritsParentSampledDecision(t *testing.T) {
	rt := &recordingTransport{}
	withSampler(t, func() float64 { return 0.9 }) // root not sampled at 0.5
	c := newTestClient(t, Config{TracesSampleRate: 0.5}, rt)

	rootCtx, rootFinish := c.StartSpan(context.Background(), "root")
	// Even though the sampler would now "keep", the child must inherit the
	// root's not-sampled decision.
	childCtx, childFinish := c.StartSpan(rootCtx, "child")
	if sc := SpanFromContext(childCtx); sc == nil || sc.Sampled {
		t.Fatalf("child Sampled = %v, want false (inherited from root)", sc)
	}
	childFinish(nil)
	rootFinish(nil)

	_ = c.Flush(context.Background())
	time.Sleep(40 * time.Millisecond)
	rt.mu.Lock()
	got := len(rt.sends)
	rt.mu.Unlock()
	if got != 0 {
		t.Fatalf("recorded %d sends, want 0 (whole trace not sampled)", got)
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

type errTest string

func (e errTest) Error() string { return string(e) }

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
