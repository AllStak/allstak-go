package allstak

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// failingTransport always fails with a transient (network-like) error, so the
// worker layer takes the persist-on-failure path. It records nothing.
type failingTransport struct{ calls atomic.Int64 }

func (f *failingTransport) send(_ context.Context, _ string, _ any) error {
	f.calls.Add(1)
	return &netErr{}
}

type netErr struct{}

func (e *netErr) Error() string { return "allstak: simulated network outage" }

// permanentTransport always fails with a permanent 4xx so the spool must DROP
// rather than persist/keep.
type permanentTransport struct{}

func (permanentTransport) send(_ context.Context, path string, _ any) error {
	return &permanentSendError{statusCode: 400, path: path, body: "bad request"}
}

// toggleTransport fails while `down` is true and records sends once it is up.
// Used to exercise drain-and-resend on the "next init".
type toggleTransport struct {
	mu    sync.Mutex
	down  atomic.Bool
	sends []recordedSend
}

func (tt *toggleTransport) send(_ context.Context, path string, payload any) error {
	if tt.down.Load() {
		return &netErr{}
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.sends = append(tt.sends, recordedSend{path: path, payload: payload})
	return nil
}

func (tt *toggleTransport) recorded() []recordedSend {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return append([]recordedSend(nil), tt.sends...)
}

// spoolTestConfig returns a config with the offline queue forced ON and
// pointed at dir, session tracking OFF, and long flush intervals so worker
// goroutines stay quiet during assertions.
func spoolTestConfig(dir string) Config {
	return Config{
		APIKey:                    "ask_live_test", // non-empty so the real transport would send
		Environment:               "test",
		Release:                   "v0.0.1-test",
		ServiceName:               "orders",
		FlushInterval:             time.Hour,
		QueueCapacity:             10,
		BatchSize:                 1, // flush each batched item immediately
		RequestTimeout:            time.Second,
		MaxRetries:                1,
		EnableAutoSessionTracking: boolPtr(false),
		EnableOfflineQueue:        boolPtr(true),
		OfflineQueueDir:           dir,
	}
}

// waitForSpoolCount polls until the spool holds want envelopes or the deadline
// elapses, then returns the final count.
func waitForSpoolCount(t *testing.T, s *spool, want int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.count() == want {
			return want
		}
		time.Sleep(10 * time.Millisecond)
	}
	return s.count()
}

func TestSpoolPersistsOnSendFailure(t *testing.T) {
	dir := t.TempDir()
	ft := &failingTransport{}
	client := newWithTransport(spoolTestConfig(dir), INGEST_HOST, ft)
	if client.spool == nil {
		t.Fatal("spool should be enabled")
	}

	client.CaptureError(ErrorPayload{ExceptionClass: "boom", Message: "x", Level: "error"})

	if got := waitForSpoolCount(t, client.spool, 1); got != 1 {
		t.Fatalf("spool count = %d, want 1 after a failed send", got)
	}
	_ = client.Close(context.Background())

	// The persisted envelope must target the errors path and carry a body.
	files, _ := os.ReadDir(dir)
	var found bool
	for _, f := range files {
		if !strings.HasPrefix(f.Name(), spoolFilePrefix) {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(dir, f.Name()))
		var env spoolEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("envelope unmarshal: %v", err)
		}
		if env.Path != pathErrors {
			t.Fatalf("envelope path = %q, want %q", env.Path, pathErrors)
		}
		if len(env.Body) == 0 {
			t.Fatal("envelope body is empty")
		}
		found = true
	}
	if !found {
		t.Fatal("no envelope file written")
	}
}

func TestSpoolDrainAndResendOnInit(t *testing.T) {
	dir := t.TempDir()

	// First "run": transport is down, so the captured error is spooled.
	tt := &toggleTransport{}
	tt.down.Store(true)
	c1 := newWithTransport(spoolTestConfig(dir), INGEST_HOST, tt)
	c1.CaptureError(ErrorPayload{ExceptionClass: "boom", Message: "outage", Level: "error"})
	waitForSpoolCount(t, c1.spool, 1)
	_ = c1.Close(context.Background())

	if n := c1.spool.count(); n != 1 {
		t.Fatalf("after first run spool count = %d, want 1", n)
	}

	// Second "run" (next init): transport is up. Drain on init must replay the
	// spooled envelope through the transport and remove it once accepted.
	tt2 := &toggleTransport{}
	c2 := newWithTransport(spoolTestConfig(dir), INGEST_HOST, tt2)
	defer c2.Close(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(tt2.recorded()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sends := tt2.recorded()
	if len(sends) == 0 {
		t.Fatal("drain on init did not replay the spooled envelope")
	}
	if sends[0].path != pathErrors {
		t.Fatalf("replayed path = %q, want %q", sends[0].path, pathErrors)
	}
	// The replayed body must round-trip back to the original error.
	body, ok := sends[0].payload.(json.RawMessage)
	if !ok {
		t.Fatalf("replayed payload type = %T, want json.RawMessage", sends[0].payload)
	}
	var ep ErrorPayload
	if err := json.Unmarshal(body, &ep); err != nil {
		t.Fatalf("replayed body unmarshal: %v", err)
	}
	if ep.Message != "outage" {
		t.Fatalf("replayed message = %q, want outage", ep.Message)
	}
	if n := waitForSpoolCount(t, c2.spool, 0); n != 0 {
		t.Fatalf("spool count after successful drain = %d, want 0", n)
	}
}

func TestSpoolScrubsBeforePersist(t *testing.T) {
	dir := t.TempDir()
	ft := &failingTransport{}
	client := newWithTransport(spoolTestConfig(dir), INGEST_HOST, ft)

	const secret = "super-secret-value-1234"
	client.CaptureError(ErrorPayload{
		ExceptionClass: "boom",
		Message:        "x",
		Level:          "error",
		Metadata: map[string]any{
			"password": secret,
			"safe":     "kept",
		},
	})
	waitForSpoolCount(t, client.spool, 1)
	_ = client.Close(context.Background())

	// Read every byte on disk and assert the secret never appears, the
	// redaction sentinel does, and the non-sensitive field is preserved.
	files, _ := os.ReadDir(dir)
	var sawEnvelope bool
	for _, f := range files {
		if !strings.HasPrefix(f.Name(), spoolFilePrefix) {
			continue
		}
		sawEnvelope = true
		raw, _ := os.ReadFile(filepath.Join(dir, f.Name()))
		text := string(raw)
		if strings.Contains(text, secret) {
			t.Fatalf("SECRET leaked to disk in %s", f.Name())
		}
		if !strings.Contains(text, redactedValue) {
			t.Fatalf("expected redaction sentinel %q on disk, got: %s", redactedValue, text)
		}
		if !strings.Contains(text, "kept") {
			t.Fatalf("non-sensitive value should be preserved on disk, got: %s", text)
		}
	}
	if !sawEnvelope {
		t.Fatal("no envelope written")
	}
}

func TestSpoolEvictsOldestWhenFull(t *testing.T) {
	dir := t.TempDir()
	s := newSpool(dir, 3, defaultSpoolMaxBytes, defaultSpoolMaxAge, nil)
	if s == nil {
		t.Fatal("spool should construct on a writable temp dir")
	}

	// Persist 5 envelopes into a 3-cap store. Oldest two must be evicted.
	for i := 0; i < 5; i++ {
		body, _ := json.Marshal(map[string]any{"seq": i})
		s.persist(pathLogs, body)
	}
	if n := s.count(); n != 3 {
		t.Fatalf("spool count = %d, want 3 (cap enforced)", n)
	}

	// Drain and confirm the SURVIVORS are the newest three (seq 2,3,4) — the
	// oldest (0,1) were dropped.
	tt := &toggleTransport{}
	s.drain(context.Background(), tt)
	seqs := map[float64]bool{}
	for _, rec := range tt.recorded() {
		body := rec.payload.(json.RawMessage)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		seqs[m["seq"].(float64)] = true
	}
	for _, want := range []float64{2, 3, 4} {
		if !seqs[want] {
			t.Fatalf("expected surviving seq %v, got %v", want, seqs)
		}
	}
	for _, gone := range []float64{0, 1} {
		if seqs[gone] {
			t.Fatalf("seq %v should have been evicted (drop oldest)", gone)
		}
	}
}

func TestSpoolDoesNotPersistSessions(t *testing.T) {
	dir := t.TempDir()
	s := newSpool(dir, defaultSpoolMaxEntries, defaultSpoolMaxBytes, defaultSpoolMaxAge, nil)

	if shouldSpoolPath(pathSessionsStart) {
		t.Fatal("sessions/start must not be spoolable")
	}
	if shouldSpoolPath(pathSessionsEnd) {
		t.Fatal("sessions/end must not be spoolable")
	}
	if shouldSpoolPath(pathHeartbeat) {
		t.Fatal("heartbeat must not be spoolable")
	}
	if shouldSpoolPath(pathReleases) {
		t.Fatal("releases must not be spoolable")
	}
	for _, p := range []string{pathErrors, pathLogs, pathHTTPRequests, pathDBQueries, pathSpans} {
		if !shouldSpoolPath(p) {
			t.Fatalf("%s must be spoolable", p)
		}
	}

	// End-to-end: a client whose session start fails (transport down) must NOT
	// leave a session envelope on disk, but a failed error WOULD be spooled.
	ft := &failingTransport{}
	cfg := spoolTestConfig(dir)
	cfg.EnableAutoSessionTracking = boolPtr(true) // force a session start attempt
	client := newWithTransport(cfg, INGEST_HOST, ft)
	client.CaptureError(ErrorPayload{ExceptionClass: "boom", Message: "x", Level: "error"})
	waitForSpoolCount(t, client.spool, 1)
	_ = client.Close(context.Background())

	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if !strings.HasPrefix(f.Name(), spoolFilePrefix) {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(dir, f.Name()))
		var env spoolEnvelope
		_ = json.Unmarshal(raw, &env)
		if env.Path == pathSessionsStart || env.Path == pathSessionsEnd {
			t.Fatalf("a session call was persisted to disk (path=%s)", env.Path)
		}
	}
	_ = s
}

func TestSpoolOptOutDisablesPersistence(t *testing.T) {
	dir := t.TempDir()
	ft := &failingTransport{}
	cfg := spoolTestConfig(dir)
	cfg.EnableOfflineQueue = boolPtr(false) // opt out
	client := newWithTransport(cfg, INGEST_HOST, ft)
	defer client.Close(context.Background())

	if client.spool != nil {
		t.Fatal("spool should be nil when offline queue is opted out")
	}

	client.CaptureError(ErrorPayload{ExceptionClass: "boom", Message: "x", Level: "error"})
	// Give the worker time to attempt + fail the send.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && ft.calls.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if strings.HasPrefix(f.Name(), spoolFilePrefix) {
			t.Fatalf("opt-out wrote an envelope to disk: %s", f.Name())
		}
	}
}

func TestSpoolPermanentFailureIsNotPersisted(t *testing.T) {
	dir := t.TempDir()
	cfg := spoolTestConfig(dir)
	client := newWithTransport(cfg, INGEST_HOST, permanentTransport{})
	defer client.Close(context.Background())

	client.CaptureError(ErrorPayload{ExceptionClass: "boom", Message: "x", Level: "error"})
	// Allow the worker to run.
	time.Sleep(200 * time.Millisecond)

	if n := client.spool.count(); n != 0 {
		t.Fatalf("permanent 4xx should not be spooled, count = %d", n)
	}
}

func TestSpoolDropsStaleEnvelopesOnDrain(t *testing.T) {
	dir := t.TempDir()
	// maxAge of 1ms so anything written is immediately stale.
	s := newSpool(dir, defaultSpoolMaxEntries, defaultSpoolMaxBytes, time.Millisecond, nil)
	body, _ := json.Marshal(map[string]any{"k": "v"})
	s.persist(pathLogs, body)
	if s.count() != 1 {
		t.Fatalf("expected 1 spooled before drain, got %d", s.count())
	}
	time.Sleep(5 * time.Millisecond)

	tt := &toggleTransport{}
	s.drain(context.Background(), tt)
	if len(tt.recorded()) != 0 {
		t.Fatalf("stale envelope should NOT be re-sent, got %d sends", len(tt.recorded()))
	}
	if n := s.count(); n != 0 {
		t.Fatalf("stale envelope should be dropped, count = %d", n)
	}
}

func TestSpoolGracefulNoOpWhenUnavailable(t *testing.T) {
	// Point the spool at a path under a read-only parent so MkdirAll/CreateTemp
	// fails. newSpool must return nil (degrade to in-memory), never panic.
	base := t.TempDir()
	roParent := filepath.Join(base, "ro")
	if err := os.Mkdir(roParent, 0o500); err != nil { // r-x only, no write
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roParent, 0o700) })

	target := filepath.Join(roParent, "spool-here")
	s := newSpool(target, 0, 0, 0, nil)
	if s != nil {
		// Some CI runs as root and can write anywhere; only assert nil when the
		// dir genuinely could not be created.
		if _, err := os.Stat(target); err == nil {
			t.Skip("running as root / dir writable; cannot exercise read-only path")
		}
		t.Fatalf("newSpool should return nil on an unwritable dir")
	}

	// All methods must be safe no-ops on the nil spool.
	s.persist(pathErrors, []byte(`{}`))
	s.drain(context.Background(), &toggleTransport{})
	if s.count() != 0 {
		t.Fatal("nil spool count should be 0")
	}

	// A client constructed against the unwritable dir must still work in-memory.
	cfg := spoolTestConfig(target)
	client := newWithTransport(cfg, INGEST_HOST, &recordingTransport{})
	defer client.Close(context.Background())
	if client.spool != nil {
		t.Fatal("client spool should be nil when dir is unwritable")
	}
	// Capture must not panic.
	client.CaptureError(ErrorPayload{ExceptionClass: "boom", Message: "x"})
}

func TestShouldEnableOfflineQueueResolution(t *testing.T) {
	// Explicit flag always wins.
	if !shouldEnableOfflineQueue(boolPtr(true)) {
		t.Fatal("explicit true should enable")
	}
	if shouldEnableOfflineQueue(boolPtr(false)) {
		t.Fatal("explicit false should disable")
	}
	// Default (nil) under the .test binary is OFF unless an env var overrides.
	t.Setenv(envOfflineQueue, "")
	if shouldEnableOfflineQueue(nil) {
		t.Fatal("default under .test binary should be off")
	}
	t.Setenv(envOfflineQueue, "1")
	if !shouldEnableOfflineQueue(nil) {
		t.Fatal("env=1 should enable")
	}
	t.Setenv(envOfflineQueue, "off")
	if shouldEnableOfflineQueue(nil) {
		t.Fatal("env=off should disable")
	}
}
