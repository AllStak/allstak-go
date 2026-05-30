package allstak

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestDiagnosticsPrivacySafeCounters(t *testing.T) {
	rt := &recordingTransport{}
	disableSessions := false
	c := newWithTransport(Config{
		APIKey:                    "k",
		Environment:               "test",
		Release:                   "1.0.0",
		EnableAutoSessionTracking: &disableSessions,
	}.applyDefaults(), INGEST_HOST, rt)
	defer c.Close(context.Background())

	c.CaptureLog(LogPayload{
		Level:   "info",
		Message: "password is secret-value",
		Metadata: map[string]any{
			"Authorization": "Bearer secret-value",
		},
	})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	d := c.GetDiagnostics()
	if d.EventsCaptured != 1 {
		t.Fatalf("eventsCaptured = %d, want 1", d.EventsCaptured)
	}
	if d.EventsSent != 1 {
		t.Fatalf("eventsSent = %d, want 1", d.EventsSent)
	}
	raw := strings.ToLower(fmt.Sprintf("%+v", d))
	if strings.Contains(raw, "secret-value") || strings.Contains(raw, "authorization") {
		t.Fatalf("diagnostics leaked payload data: %s", raw)
	}
}

func TestDiagnosticsCountsPersistentSpool(t *testing.T) {
	dir := t.TempDir()
	ft := &failingTransport{}
	c := newWithTransport(spoolTestConfig(dir), INGEST_HOST, ft)
	defer c.Close(context.Background())

	c.CaptureLog(LogPayload{Level: "info", Message: "outage"})
	waitForSpoolCount(t, c.spool, 1)

	d := c.GetDiagnostics()
	if d.EventsFailed != 1 {
		t.Fatalf("eventsFailed = %d, want 1", d.EventsFailed)
	}
	if d.EventsPersisted != 1 {
		t.Fatalf("eventsPersisted = %d, want 1", d.EventsPersisted)
	}
	if d.EventsDropped != 0 {
		t.Fatalf("eventsDropped = %d, want 0", d.EventsDropped)
	}
	if d.QueueSize != 1 {
		t.Fatalf("queueSize = %d, want 1", d.QueueSize)
	}
}
