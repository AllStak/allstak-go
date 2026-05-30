package allstak

import (
	"math/rand"
	"sync"
)

// Sampling + BeforeSend pipeline.
//
// This file owns the two head-of-pipeline knobs that decide whether an event
// ever reaches the transport:
//
//   - SampleRate          — deterministic random drop of error/message events.
//   - BeforeSend          — a user callback that can mutate or drop an event.
//   - TracesSampleRate    — random drop of span creation, with the decision
//     reflected in the propagated W3C traceparent sampled flag.
//
// The ordering invariant (documented on Client.applyPipeline below) is:
//
//	SampleRate drop  →  pre-hook sanitizer  →  BeforeSend  →  final sanitizer  →  transport
//
// The sanitizer runs before BeforeSend and again at the single wire chokepoint
// in transport.send. Hooks receive a sanitized event, and any values reintroduced
// by a hook are sanitized again before persistence/network send.

// sampleFunc is the seam tests override to make sampling deterministic. It
// returns a float64 in [0.0, 1.0) — the same contract as math/rand.Float64.
// Package-level (not per-Client) because it is a pure RNG source; a test that
// overrides it must restore it (see withSampler in the tests).
//
// Guarded by sampleMu so a test swapping it cannot race a concurrent capture
// on another goroutine.
var (
	sampleMu   sync.RWMutex
	sampleFunc = rand.Float64
)

// drawSample returns one sample in [0.0, 1.0) using the (possibly overridden)
// sampler. Read-locked so it composes safely with a test swap.
func drawSample() float64 {
	sampleMu.RLock()
	f := sampleFunc
	sampleMu.RUnlock()
	return f()
}

// effectiveSampleRate normalizes the SampleRate config value.
//
// Zero-value decision (documented): a zero SampleRate is treated as "unset →
// 1.0 (keep everything)". This matches how every other field in Config treats
// its zero value (applyDefaults fills unset durations/sizes with sane
// defaults) and means simply constructing a Config does not silently start
// dropping errors — the safe, least-surprising default for an error monitor.
// Out-of-range values are clamped to [0.0, 1.0].
func effectiveSampleRate(rate float64) float64 {
	if rate <= 0 {
		// 0 (or negative / unset) means "no sampling configured" → keep all.
		return 1.0
	}
	if rate >= 1 {
		return 1.0
	}
	return rate
}

// shouldSampleError reports whether an error/message event survives the
// SampleRate gate. rate is the raw Config.SampleRate; normalization happens
// here. A rate of 1.0 (the default) always keeps the event without drawing
// from the RNG, so the common case stays allocation- and lock-free.
func shouldSampleError(rate float64) bool {
	r := effectiveSampleRate(rate)
	if r >= 1.0 {
		return true
	}
	return drawSample() < r
}

// shouldSampleTrace reports whether a new span should be created/recorded.
//
// Zero-value decision (documented): TracesSampleRate is a plain float64 whose
// zero value means "tracing sampling disabled → keep all spans". We chose a
// float64 (not *float64) for symmetry with SampleRate and because spans are
// already opt-in (you only get them if you call StartSpan or wire an
// integration), so the safe default is to record what the caller explicitly
// asked to trace. A rate in (0,1) samples; >=1 keeps all; a negative rate is
// treated as unset (keep all).
func shouldSampleTrace(rate float64) bool {
	if rate <= 0 {
		// Unset / disabled → record everything the caller explicitly traces.
		return true
	}
	if rate >= 1 {
		return true
	}
	return drawSample() < rate
}

// applyBeforeSend runs the user's BeforeSend callback (if configured) against
// a sanitized copy of the payload. It is FAIL-OPEN: if the callback panics, we
// recover, log under Debug, and fall back to sending the pre-sanitized event.
// Returning nil from the callback DROPS the event (returns nil here).
func (c *Client) applyBeforeSend(p *ErrorPayload) (out *ErrorPayload) {
	sanitized, ok := scrubPayloadSafe(p, c.cfg.scrubOptions()).(*ErrorPayload)
	if !ok || sanitized == nil {
		sanitized = p
	}
	if c.cfg.BeforeSend == nil {
		return sanitized
	}
	// Fail-open guard: a panicking callback yields the sanitized event.
	out = sanitized
	defer func() {
		if r := recover(); r != nil {
			c.debugf("BeforeSend panicked, sending sanitized event: %v", r)
			out = sanitized
		}
	}()
	return c.cfg.BeforeSend(sanitized)
}
