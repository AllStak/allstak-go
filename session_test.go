package allstak

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func boolPtr(b bool) *bool { return &b }

// sessionTestConfig returns a config that forces session tracking ON (the
// default guard skips it under the ".test" binary) with long flush intervals
// so the worker goroutines stay quiet during assertions.
func sessionTestConfig() Config {
	return Config{
		Environment:               "test",
		Release:                   "v0.0.1-test",
		ServiceName:               "orders",
		FlushInterval:             time.Hour,
		QueueCapacity:             10,
		BatchSize:                 10,
		RequestTimeout:            time.Second,
		SDKName:                   "allstak-go",
		SDKVersion:                "9.9.9",
		Platform:                  "go",
		EnableAutoSessionTracking: boolPtr(true),
	}
}

func sessionRecoveryTestConfig(t *testing.T) Config {
	cfg := sessionTestConfig()
	cfg.OfflineQueueDir = t.TempDir()
	return cfg
}

// findSend returns the first recorded send to path, or false.
func findSend(sends []recordedSend, path string) (recordedSend, bool) {
	for _, s := range sends {
		if s.path == path {
			return s, true
		}
	}
	return recordedSend{}, false
}

func TestSessionStartPayloadShape(t *testing.T) {
	rt := &recordingTransport{}
	client := newWithTransport(sessionTestConfig(), INGEST_HOST, rt)
	defer client.Close(context.Background())

	sends := rt.waitFor(t, 1)
	start, ok := findSend(sends, pathSessionsStart)
	if !ok {
		t.Fatalf("no send to %s; got %+v", pathSessionsStart, sends)
	}
	p, ok := start.payload.(sessionStartPayload)
	if !ok {
		t.Fatalf("start payload type = %T, want sessionStartPayload", start.payload)
	}
	if p.SessionID == "" {
		t.Fatalf("start payload missing sessionId")
	}
	if p.Release != "v0.0.1-test" {
		t.Fatalf("Release = %q, want v0.0.1-test", p.Release)
	}
	if p.Environment != "test" {
		t.Fatalf("Environment = %q, want test", p.Environment)
	}
	if p.SDKName != "allstak-go" {
		t.Fatalf("SDKName = %q, want allstak-go", p.SDKName)
	}
	if p.SDKVersion != "9.9.9" {
		t.Fatalf("SDKVersion = %q, want 9.9.9", p.SDKVersion)
	}
	if p.Platform != "go" {
		t.Fatalf("Platform = %q, want go", p.Platform)
	}
}

func TestSessionStartReleaseFallsBackToSDKVersion(t *testing.T) {
	rt := &recordingTransport{}
	cfg := sessionTestConfig()
	cfg.Release = "" // simulate no release resolved
	cfg.SDKVersion = "7.7.7"
	client := newWithTransport(cfg, INGEST_HOST, rt)
	defer client.Close(context.Background())

	sends := rt.waitFor(t, 1)
	start, ok := findSend(sends, pathSessionsStart)
	if !ok {
		t.Fatalf("no send to %s", pathSessionsStart)
	}
	p := start.payload.(sessionStartPayload)
	if p.Release != "7.7.7" {
		t.Fatalf("Release = %q, want SDKVersion fallback 7.7.7", p.Release)
	}
}

func TestSessionStartStampsUserID(t *testing.T) {
	rt := &recordingTransport{}
	cfg := sessionTestConfig()
	cfg.User = &UserContext{ID: "user-42"}
	client := newWithTransport(cfg, INGEST_HOST, rt)
	defer client.Close(context.Background())

	sends := rt.waitFor(t, 1)
	start, _ := findSend(sends, pathSessionsStart)
	p := start.payload.(sessionStartPayload)
	if p.UserID != "user-42" {
		t.Fatalf("UserID = %q, want user-42", p.UserID)
	}
}

func TestSessionEndStatusOKWhenNoErrors(t *testing.T) {
	rt := &recordingTransport{}
	client := newWithTransport(sessionTestConfig(), INGEST_HOST, rt)

	// Wait for start so the session id is recorded before we close.
	rt.waitFor(t, 1)
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sends := rt.waitFor(t, 2)
	end, ok := findSend(sends, pathSessionsEnd)
	if !ok {
		t.Fatalf("no send to %s; got %+v", pathSessionsEnd, sends)
	}
	p, ok := end.payload.(sessionEndPayload)
	if !ok {
		t.Fatalf("end payload type = %T, want sessionEndPayload", end.payload)
	}
	if p.Status != statusOK {
		t.Fatalf("Status = %q, want ok", p.Status)
	}
	if p.SessionID == "" {
		t.Fatalf("end payload missing sessionId")
	}
	if p.DurationMs < 0 {
		t.Fatalf("DurationMs = %d, want >= 0", p.DurationMs)
	}
}

func TestSessionEndStatusErroredAfterHandledError(t *testing.T) {
	rt := &recordingTransport{}
	client := newWithTransport(sessionTestConfig(), INGEST_HOST, rt)
	rt.waitFor(t, 1)

	client.CaptureError(ErrorPayload{ExceptionClass: "boom", Message: "x", Level: "error"})
	client.CaptureError(ErrorPayload{ExceptionClass: "boom", Message: "y", Level: "error"})

	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sends := rt.waitFor(t, 2)
	end, ok := findSend(sends, pathSessionsEnd)
	if !ok {
		t.Fatalf("no send to %s", pathSessionsEnd)
	}
	if p := end.payload.(sessionEndPayload); p.Status != statusErrored {
		t.Fatalf("Status = %q, want errored", p.Status)
	}
}

func TestSessionEndStatusCrashedAfterFatal(t *testing.T) {
	rt := &recordingTransport{}
	client := newWithTransport(sessionTestConfig(), INGEST_HOST, rt)
	rt.waitFor(t, 1)

	// errored first, then crashed — crash must win (no downgrade).
	client.CaptureError(ErrorPayload{ExceptionClass: "boom", Message: "x", Level: "error"})
	client.CaptureError(ErrorPayload{ExceptionClass: "panic", Message: "fatal", Level: "fatal"})

	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sends := rt.waitFor(t, 2)
	end, _ := findSend(sends, pathSessionsEnd)
	if p := end.payload.(sessionEndPayload); p.Status != statusCrashed {
		t.Fatalf("Status = %q, want crashed", p.Status)
	}
}

func TestSessionCrashedNeverDowngrades(t *testing.T) {
	rt := &recordingTransport{}
	client := newWithTransport(sessionTestConfig(), INGEST_HOST, rt)
	rt.waitFor(t, 1)

	// crash first, then a later handled error must NOT downgrade to errored.
	client.CaptureError(ErrorPayload{ExceptionClass: "panic", Message: "fatal", Level: "fatal"})
	client.CaptureError(ErrorPayload{ExceptionClass: "boom", Message: "x", Level: "error"})

	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sends := rt.waitFor(t, 2)
	end, _ := findSend(sends, pathSessionsEnd)
	if p := end.payload.(sessionEndPayload); p.Status != statusCrashed {
		t.Fatalf("Status = %q, want crashed (no downgrade)", p.Status)
	}
}

func TestSessionIDAttachedToErrorPayload(t *testing.T) {
	rt := &recordingTransport{}
	client := newWithTransport(sessionTestConfig(), INGEST_HOST, rt)
	defer client.Close(context.Background())

	// Drain the start send so the next recorded send is the error.
	rt.waitFor(t, 1)
	wantID := client.sessionID()
	if wantID == "" {
		t.Fatalf("session id should be set after start")
	}

	client.CaptureError(ErrorPayload{ExceptionClass: "boom", Message: "x", Level: "error"})

	sends := rt.waitFor(t, 2)
	errSend, ok := findSend(sends, pathErrors)
	if !ok {
		t.Fatalf("no send to %s", pathErrors)
	}
	p := errSend.payload.(*ErrorPayload)
	if p.SessionID != wantID {
		t.Fatalf("error SessionID = %q, want %q", p.SessionID, wantID)
	}
}

func TestSessionEndIsIdempotent(t *testing.T) {
	rt := &recordingTransport{}
	client := newWithTransport(sessionTestConfig(), INGEST_HOST, rt)
	rt.waitFor(t, 1)

	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Second Close is a no-op (closeOnce); endSession also has its own once.
	_ = client.Close(context.Background())
	client.endSession()

	// Exactly one start + one end recorded.
	sends := rt.waitFor(t, 2)
	count := 0
	for _, s := range sends {
		if s.path == pathSessionsEnd {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("recorded %d session-end sends, want exactly 1", count)
	}
}

func TestSessionRecoveryCleanShutdownDoesNotReportAbnormal(t *testing.T) {
	cfg := sessionRecoveryTestConfig(t)
	rt1 := &recordingTransport{}
	client1 := newWithTransport(cfg, INGEST_HOST, rt1)
	rt1.waitFor(t, 1)
	if err := client1.Close(context.Background()); err != nil {
		t.Fatalf("Close first: %v", err)
	}

	rt2 := &recordingTransport{}
	client2 := newWithTransport(cfg, INGEST_HOST, rt2)
	defer client2.Close(context.Background())
	sends := rt2.waitFor(t, 1)
	for _, s := range sends {
		if s.path == pathSessionsEnd {
			t.Fatalf("clean shutdown recovered an abnormal session: %+v", s.payload)
		}
	}
}

func TestSessionRecoveryOpenSessionReportsAbnormal(t *testing.T) {
	cfg := sessionRecoveryTestConfig(t)
	rt1 := &recordingTransport{}
	client1 := newWithTransport(cfg, INGEST_HOST, rt1)
	_ = client1 // intentionally left open to simulate a killed process
	rt1.waitFor(t, 1)
	previousID := client1.sessionID()

	rt2 := &recordingTransport{}
	client2 := newWithTransport(cfg, INGEST_HOST, rt2)
	defer client2.Close(context.Background())
	sends := rt2.waitFor(t, 2)
	end, ok := findSend(sends, pathSessionsEnd)
	if !ok {
		t.Fatalf("no recovered session end; got %+v", sends)
	}
	p := end.payload.(sessionEndPayload)
	if p.SessionID != previousID {
		t.Fatalf("recovered SessionID = %q, want %q", p.SessionID, previousID)
	}
	if p.Status != statusAbnormal {
		t.Fatalf("recovered Status = %q, want abnormal", p.Status)
	}
}

func TestSessionRecoveryCrashedSessionReportsCrashed(t *testing.T) {
	cfg := sessionRecoveryTestConfig(t)
	rt1 := &recordingTransport{}
	client1 := newWithTransport(cfg, INGEST_HOST, rt1)
	rt1.waitFor(t, 1)
	previousID := client1.sessionID()
	client1.CaptureError(ErrorPayload{ExceptionClass: "panic", Message: "fatal", Level: "fatal"})

	rt2 := &recordingTransport{}
	client2 := newWithTransport(cfg, INGEST_HOST, rt2)
	defer client2.Close(context.Background())
	sends := rt2.waitFor(t, 2)
	end, ok := findSend(sends, pathSessionsEnd)
	if !ok {
		t.Fatalf("no recovered session end; got %+v", sends)
	}
	p := end.payload.(sessionEndPayload)
	if p.SessionID != previousID {
		t.Fatalf("recovered SessionID = %q, want %q", p.SessionID, previousID)
	}
	if p.Status != statusCrashed {
		t.Fatalf("recovered Status = %q, want crashed", p.Status)
	}
}

func TestSessionRecoveryCorruptStateDoesNotCrash(t *testing.T) {
	cfg := sessionRecoveryTestConfig(t)
	path := sessionStatePath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	rt := &recordingTransport{}
	client := newWithTransport(cfg, INGEST_HOST, rt)
	defer client.Close(context.Background())
	sends := rt.waitFor(t, 1)
	for _, s := range sends {
		if s.path == pathSessionsEnd {
			t.Fatalf("corrupt state emitted session end: %+v", s.payload)
		}
	}
}

func TestSessionRecoveryDoesNotDuplicateRecoveredAbnormal(t *testing.T) {
	cfg := sessionRecoveryTestConfig(t)
	rt1 := &recordingTransport{}
	client1 := newWithTransport(cfg, INGEST_HOST, rt1)
	_ = client1 // intentionally left open to simulate a killed process
	rt1.waitFor(t, 1)

	rt2 := &recordingTransport{}
	client2 := newWithTransport(cfg, INGEST_HOST, rt2)
	rt2.waitFor(t, 2)
	if err := client2.Close(context.Background()); err != nil {
		t.Fatalf("Close second: %v", err)
	}

	rt3 := &recordingTransport{}
	client3 := newWithTransport(cfg, INGEST_HOST, rt3)
	defer client3.Close(context.Background())
	sends := rt3.waitFor(t, 1)
	for _, s := range sends {
		if s.path == pathSessionsEnd {
			p := s.payload.(sessionEndPayload)
			if p.Status == statusAbnormal {
				t.Fatalf("duplicated recovered abnormal session: %+v", p)
			}
		}
	}
}

func TestSessionTrackingDisabledOptOut(t *testing.T) {
	rt := &recordingTransport{}
	cfg := sessionTestConfig()
	cfg.EnableAutoSessionTracking = boolPtr(false)
	client := newWithTransport(cfg, INGEST_HOST, rt)

	if client.session != nil {
		t.Fatalf("session tracker should be nil when opted out")
	}
	if id := client.sessionID(); id != "" {
		t.Fatalf("sessionID = %q, want empty when opted out", id)
	}

	// Capturing an error must not start a session or stamp a session id.
	client.CaptureError(ErrorPayload{ExceptionClass: "boom", Message: "x", Level: "error"})
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sends := rt.waitFor(t, 1)
	for _, s := range sends {
		if s.path == pathSessionsStart || s.path == pathSessionsEnd {
			t.Fatalf("opt-out emitted session send to %s", s.path)
		}
		if errp, ok := s.payload.(*ErrorPayload); ok && errp.SessionID != "" {
			t.Fatalf("opt-out stamped SessionID %q on error", errp.SessionID)
		}
	}
}

func TestShouldTrackSessionsDefaultSkipsUnderTestBinary(t *testing.T) {
	// os.Args[0] ends in ".test" under `go test`, so the nil-default guard
	// must report false — mirroring the Java SDK's unit-test skip.
	if shouldTrackSessions(nil) {
		t.Fatalf("shouldTrackSessions(nil) = true under test binary, want false")
	}
	if !shouldTrackSessions(boolPtr(true)) {
		t.Fatalf("explicit true should force tracking on")
	}
	if shouldTrackSessions(boolPtr(false)) {
		t.Fatalf("explicit false should force tracking off")
	}
}
