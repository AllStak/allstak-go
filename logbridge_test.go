package allstak

import (
	"context"
	"testing"
)

func TestBridgeLogShipsLogOnly(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{APIKey: "ask_test"}, rt)

	c.BridgeLog(context.Background(), BridgeRecord{
		Level:   "info",
		Message: "started",
		Fields:  map[string]any{"k": "v"},
	})

	sends := rt.waitFor(t, 1)
	p, ok := sends[0].payload.(*LogPayload)
	if !ok {
		t.Fatalf("payload type = %T, want *LogPayload", sends[0].payload)
	}
	if p.Level != "info" || p.Message != "started" {
		t.Fatalf("unexpected log payload: %#v", p)
	}
	if p.Metadata["k"] != "v" {
		t.Fatalf("metadata not carried: %#v", p.Metadata)
	}
}

func TestBridgeLogPromotesErrorWithErr(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{APIKey: "ask_test"}, rt)

	c.BridgeLog(context.Background(), BridgeRecord{
		Level:   "error",
		Message: "charge failed",
		Err:     errTestBridge("db down"),
	})

	// Expect TWO sends: one log, one error.
	sends := rt.waitFor(t, 2)
	var sawLog, sawErr bool
	for _, s := range sends {
		switch p := s.payload.(type) {
		case *LogPayload:
			if p.Level == "error" {
				sawLog = true
			}
		case *ErrorPayload:
			if p.Message == "db down" {
				sawErr = true
			}
		}
	}
	if !sawLog {
		t.Fatal("expected the error-level log to be shipped")
	}
	if !sawErr {
		t.Fatal("expected the attached error to be promoted to the errors stream")
	}
}

func TestBridgeLogDoesNotPromoteErrorWithoutErr(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{APIKey: "ask_test"}, rt)

	// Error level but NO error value attached — should ship a log, not an error.
	c.BridgeLog(context.Background(), BridgeRecord{
		Level:   "error",
		Message: "noisy error log with no err",
	})

	sends := rt.waitFor(t, 1)
	for _, s := range sends {
		if _, ok := s.payload.(*ErrorPayload); ok {
			t.Fatalf("did not expect an error promotion: %#v", s.payload)
		}
	}
}

func TestNormalizeBridgeLevel(t *testing.T) {
	cases := map[string]string{
		"TRACE": "debug", "Debug": "debug",
		"INFO": "info", "": "info", "notice": "info",
		"WARN": "warn", "Warning": "warn",
		"ERROR": "error", "fatal": "error", "panic": "error", "dpanic": "error",
		"weird": "info",
	}
	for in, want := range cases {
		if got := normalizeBridgeLevel(in); got != want {
			t.Errorf("normalizeBridgeLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

type errTestBridge string

func (e errTestBridge) Error() string { return string(e) }
