package allstak

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// sdkVersion is stamped into the User-Agent header and into the wire
// sdk.version. It aliases the single source of truth in config.go (SDKVersion)
// so the User-Agent and the wire metadata can never disagree.
const sdkVersion = SDKVersion

// Client is the central entry point for the SDK. It owns a background worker
// goroutine per ingest stream (errors, logs, requests, db, spans) that
// batches and flushes events to the AllStak backend.
//
// A Client is safe for concurrent use and is designed to be created once at
// program start and reused for the process lifetime. Call Close (or Flush)
// before exit so buffered events are not lost.
type Client struct {
	cfg       Config
	transport ingestTransport
	host      string

	// One queue + one worker per stream keeps head-of-line blocking from
	// one signal type (e.g. a chatty DB) from starving another (e.g. rare
	// but critical errors).
	errs     chan *ErrorPayload
	logs     chan *LogPayload
	requests chan *HTTPRequestItem
	dbq      chan *DBQueryItem
	spans    chan *SpanItem

	// session tracks the single release-health session for this process
	// (start on New, end on Close). nil when auto session tracking is disabled.
	session *sessionTracker

	// spool is the persistent (offline) event store. Events that fail to send
	// (outage/retries-exhausted) or are still buffered at shutdown are written
	// here, scrubbed, and replayed on the next init. nil when the offline queue
	// is disabled or the cache dir is unavailable — every spool method no-ops on
	// a nil receiver so the rest of the client never nil-checks.
	spool *spool

	// Lifecycle
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    atomic.Bool

	// sendMu guards the enqueue→channel-send window against Close. Every
	// capture path takes the read lock and re-checks `closed` before sending;
	// Close sets `closed` and takes the write lock BEFORE closing the
	// channels. This removes the TOCTOU between the lock-free closed.Load()
	// fast path and close(chan) — a sender that passed the fast path either
	// completes its send before Close acquires the write lock, or observes
	// closed==true under the read lock and bails. Without this a descheduled
	// sender could panic with "send on closed channel" and crash the host.
	sendMu sync.RWMutex

	// Counters exposed via Stats() for observability/tests.
	captured          atomic.Int64
	sent              atomic.Int64
	dropped           atomic.Int64
	failed            atomic.Int64
	persisted         atomic.Int64
	replayed          atomic.Int64
	sessionRecoveries atomic.Int64
}

// ingestTransport is the minimal contract the client needs from its
// transport. It's an interface so tests can inject a fake without spinning
// up an HTTP server.
type ingestTransport interface {
	send(ctx context.Context, path string, payload any) error
}

// New constructs a Client with the default HTTP transport. If APIKey is
// empty the client is in no-op mode: capture APIs accept input but nothing
// is sent. This is intentional so libraries that wire AllStak in can be
// safely imported without configuration.
//
// New never fails: if the resolved ingest host is rejected by the security
// validation (a non-loopback http:// host without AllowInsecureHost — see
// Init), New logs a loud error to stderr and returns a DISABLED no-op client
// instead of shipping the API key and telemetry in cleartext. Use Init when
// you want the validation error surfaced.
func New(cfg Config) *Client {
	c, err := Init(cfg)
	if err == nil {
		return c
	}
	// Fail-open, never crash the host: loud message + inert no-op client.
	fmt.Fprintf(os.Stderr, "[allstak] %v — SDK disabled (no-op)\n", err)
	off := false
	cfg.APIKey = "" // no-op mode: capture APIs accept input, nothing is sent
	cfg.EnableOfflineQueue = &off
	cfg.EnableAutoSessionTracking = &off
	cfg.AutoRegisterRelease = &off
	cfg = cfg.applyDefaults()
	return newWithTransport(cfg, resolveHost(), TransportFunc(func(context.Context, string, any) error { return nil }))
}

// NewWithTransport lets advanced users (and tests) inject a custom
// transport. Most applications should use New. Because the injected
// transport owns delivery, the ingest-host security validation that New/Init
// perform does not apply here.
func NewWithTransport(cfg Config, t TransportFunc) *Client {
	cfg = cfg.applyDefaults()
	host := resolveHost()
	return newWithTransport(cfg, host, t)
}

// TransportFunc adapts a plain function to the ingestTransport interface
// so tests can express transports inline without a struct.
type TransportFunc func(ctx context.Context, path string, payload any) error

func (f TransportFunc) send(ctx context.Context, path string, payload any) error {
	return f(ctx, path, payload)
}

func newWithTransport(cfg Config, host string, transport ingestTransport) *Client {
	if cfg.Debug {
		setDebugWriter(os.Stderr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		cfg:       cfg,
		transport: transport,
		host:      host,
		errs:      make(chan *ErrorPayload, cfg.QueueCapacity),
		logs:      make(chan *LogPayload, cfg.QueueCapacity),
		requests:  make(chan *HTTPRequestItem, cfg.QueueCapacity),
		dbq:       make(chan *DBQueryItem, cfg.QueueCapacity),
		spans:     make(chan *SpanItem, cfg.QueueCapacity),
		ctx:       ctx,
		cancel:    cancel,
	}

	// Errors are single-event POSTs — each is drained and sent individually
	// so stack traces reach the dashboard as fast as possible.
	c.wg.Add(1)
	go runSingleWorker(c, c.errs, pathErrors)

	// Logs are also single-event for the same reason.
	c.wg.Add(1)
	go runSingleWorker(c, c.logs, pathLogs)

	// HTTP, DB, and spans are batched — they can be extremely high volume
	// so batching is a meaningful reduction in ingress cost.
	c.wg.Add(1)
	go runBatchWorker(c, c.requests, pathHTTPRequests, func(batch []*HTTPRequestItem) any {
		items := make([]HTTPRequestItem, len(batch))
		for i, p := range batch {
			items[i] = *p
		}
		return HTTPRequestBatch{Requests: items}
	})

	c.wg.Add(1)
	go runBatchWorker(c, c.dbq, pathDBQueries, func(batch []*DBQueryItem) any {
		items := make([]DBQueryItem, len(batch))
		for i, p := range batch {
			items[i] = *p
		}
		return DBQueryBatch{Queries: items}
	})

	c.wg.Add(1)
	go runBatchWorker(c, c.spans, pathSpans, func(batch []*SpanItem) any {
		items := make([]SpanItem, len(batch))
		for i, p := range batch {
			items[i] = *p
		}
		return SpanBatch{Spans: items}
	})

	// Offline durability: construct the persistent spool and replay any events
	// left over from a previous run (a prior outage or crash). Fail-open — if
	// the cache dir is unavailable the spool stays nil and the SDK keeps its
	// existing in-memory behavior. Drain runs in the background so init never
	// blocks the host on disk or network I/O.
	if shouldEnableOfflineQueue(cfg.EnableOfflineQueue) {
		dir := resolveSpoolDir(cfg.OfflineQueueDir)
		c.spool = newSpool(dir, cfg.OfflineQueueMaxEntries, cfg.OfflineQueueMaxBytes, cfg.OfflineQueueMaxAge, c.debugf)
		if c.spool != nil {
			c.wg.Add(1)
			go func() {
				defer c.wg.Done()
				defer func() { _ = recover() }() // never let a drain crash boot
				c.spool.drain(c.ctx, c.transport, func() { c.replayed.Add(1) })
			}()
		}
	}

	c.debugf("client initialized: host=%s env=%s service=%s release=%s", host, cfg.Environment, cfg.ServiceName, cfg.Release)
	c.registerRuntimeRelease()

	// Release-health: open one session per process. Fail-open — never block
	// or panic on init. Sessions are NEVER sampled.
	if shouldTrackSessions(cfg.EnableAutoSessionTracking) {
		c.session = &sessionTracker{statePath: sessionStatePath(cfg)}
		c.startSession()
	}
	return c
}

func (c *Client) registerRuntimeRelease() {
	if c.cfg.APIKey == "" || c.cfg.Release == "" || !shouldRegisterRuntimeRelease(c.cfg.AutoRegisterRelease) {
		return
	}
	payload := map[string]any{
		"version":     c.cfg.Release,
		"environment": c.cfg.Environment,
		"commitSha":   c.cfg.CommitSha,
		"branch":      c.cfg.Branch,
		"author":      c.cfg.SDKName + "/" + c.cfg.SDKVersion,
		"message":     "Registered automatically by AllStak Go SDK at runtime",
	}
	go func() {
		defer func() { _ = recover() }() // never let release registration crash the host
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.RequestTimeout)
		defer cancel()
		_ = c.transport.send(ctx, pathReleases, payload)
	}()
}

func shouldRegisterRuntimeRelease(flag *bool) bool {
	if flag != nil {
		return *flag
	}
	return !strings.HasSuffix(os.Args[0], ".test")
}

// Config returns a copy of the resolved config (after defaults are applied).
// This is primarily useful for integrations that want to read ServiceName
// or Environment to stamp their own payloads.
func (c *Client) Config() Config { return c.cfg }

// Host returns the resolved ingest host. Primarily useful in debug logging
// and SDK self-tests.
func (c *Client) Host() string { return c.host }

// Stats returns a snapshot of the internal counters. Useful for tests and
// for end-of-run summaries in scripts.
type Stats struct {
	Sent      int64
	Dropped   int64
	Failed    int64
	Captured  int64
	Persisted int64
	Replayed  int64
}

// Stats returns a point-in-time copy of the SDK's send counters.
func (c *Client) Stats() Stats {
	return Stats{
		Sent:      c.sent.Load(),
		Dropped:   c.dropped.Load(),
		Failed:    c.failed.Load(),
		Captured:  c.captured.Load(),
		Persisted: c.persisted.Load(),
		Replayed:  c.replayed.Load(),
	}
}

// GetDiagnostics returns privacy-safe SDK counters and queue sizes.
func (c *Client) GetDiagnostics() Diagnostics {
	var td transportDiagnostics
	if d, ok := c.transport.(diagnosticTransport); ok {
		td = d.diagnostics()
	}
	return Diagnostics{
		EventsCaptured:        c.captured.Load(),
		EventsSent:            c.sent.Load(),
		EventsFailed:          c.failed.Load(),
		EventsDropped:         c.dropped.Load(),
		EventsPersisted:       c.persisted.Load(),
		EventsReplayed:        c.replayed.Load(),
		QueueSize:             len(c.errs) + len(c.logs) + len(c.requests) + len(c.dbq) + len(c.spans) + c.spool.count(),
		RetryAttempts:         td.RetryAttempts,
		RateLimitedCount:      td.RateLimitedCount,
		CompressedPayloads:    td.Compressed,
		UncompressedPayloads:  td.Uncompressed,
		CompressionBytesSaved: td.BytesSaved,
		ActiveTraceCount:      0,
		ActiveSpanCount:       0,
		BreadcrumbCount:       0,
		SessionRecoveryCount:  c.sessionRecoveries.Load(),
		Disabled:              c.closed.Load() || td.Disabled,
	}
}

// mergeReleaseTags returns a metadata map that contains all release-tracking
// tags (sdk.name/version, platform, dist, commit.sha/branch) plus any caller-
// supplied entries. Caller values win on key collision.
//
// We always allocate a fresh map when there are tags to merge so the caller's
// original metadata can never be mutated under their feet (important when the
// same payload struct is reused across goroutines).
func mergeReleaseTags(callerMeta map[string]any, tags map[string]string) map[string]any {
	if len(tags) == 0 {
		return callerMeta
	}
	out := make(map[string]any, len(tags)+len(callerMeta))
	for k, v := range tags {
		out[k] = v
	}
	for k, v := range callerMeta {
		out[k] = v
	}
	return out
}

// ── Send paths (public capture API) ───────────────────────────────────────

// guardCapture is the deferred belt-and-suspenders panic guard installed at
// every public capture entry point. The SDK's prime directive is to NEVER
// crash the host application: even if a bug slips past the lock-based
// close/send synchronization (or enrichment code panics on a pathological
// payload), the panic is swallowed here and the event is counted as dropped.
func (c *Client) guardCapture() {
	if r := recover(); r != nil {
		c.dropped.Add(1)
		c.debugf("capture panic suppressed (event dropped): %v", r)
	}
}

// enqueue sends item to ch with drop-oldest ring semantics, synchronized
// against Close:
//
//   - the read lock is held across the closed re-check AND the channel send,
//     so Close (which sets closed and then takes the write lock before
//     closing the channels) can never close a channel while a send is in
//     flight;
//   - a deferred recover() is kept as a last line of defense — a send on a
//     closed channel (impossible by construction) would be swallowed and
//     counted as a drop instead of crashing the host.
//
// It is a free generic function (not a method) because Go does not support
// type parameters on methods.
func enqueue[T any](c *Client, ch chan *T, item *T) {
	defer func() {
		if r := recover(); r != nil {
			c.dropped.Add(1)
			c.debugf("enqueue panic suppressed (event dropped): %v", r)
		}
	}()

	c.sendMu.RLock()
	defer c.sendMu.RUnlock()
	if c.closed.Load() {
		c.dropped.Add(1)
		return
	}

	select {
	case ch <- item:
	default:
		// Drop oldest and enqueue — ring-buffer semantics.
		select {
		case <-ch:
			c.dropped.Add(1)
		default:
		}
		select {
		case ch <- item:
		default:
			c.dropped.Add(1)
		}
	}
}

// CaptureError enqueues an error payload. Safe to call from any goroutine.
// Returns immediately; the actual ingest happens asynchronously.
//
// If the queue is full the oldest buffered error is dropped to make room.
// This preserves the most-recent failure which is usually the most useful
// for debugging.
func (c *Client) CaptureError(p ErrorPayload) {
	defer c.guardCapture()
	if c.closed.Load() {
		return
	}
	c.captured.Add(1)
	// Stamp env/release if caller left them blank so integrations can be
	// dumb and still get correctly-tagged events.
	if p.Environment == "" {
		p.Environment = c.cfg.Environment
	}
	if p.Release == "" {
		p.Release = c.cfg.Release
	}
	// Merge release-tracking tags (sdk.name/version, platform, dist,
	// commit.sha/branch) into Metadata. Caller-supplied metadata wins.
	p.Metadata = mergeReleaseTags(p.Metadata, c.cfg.ReleaseTags())

	// Attach the active session id so the backend's error consumer can mark
	// the session errored/crashed for crash-free release health. Caller-set
	// ids win (e.g. a request-mode integration).
	if p.SessionID == "" {
		p.SessionID = c.sessionID()
	}

	// Update the in-memory session status. A fatal-level event is an
	// unhandled/crash; any other error-level event is a handled error. This
	// happens before sampling so release-health reflects what actually
	// occurred even when the event itself is sampled out.
	if p.Level == "fatal" {
		c.recordSessionCrash()
	} else {
		c.recordSessionError()
	}

	// Pipeline order: SampleRate drop first (skip dropped events entirely),
	// then a pre-hook sanitizer, then BeforeSend (may mutate or drop). The PII
	// sanitizer runs again at the wire chokepoint in transport.send, so it
	// always sees the final post-BeforeSend payload.
	if !shouldSampleError(c.cfg.SampleRate) {
		c.dropped.Add(1)
		return
	}
	event := c.applyBeforeSend(&p)
	if event == nil {
		// BeforeSend returned nil — drop the event.
		c.dropped.Add(1)
		return
	}
	p = *event

	enqueue(c, c.errs, &p)
}

// CaptureLog enqueues a structured log event.
func (c *Client) CaptureLog(p LogPayload) {
	defer c.guardCapture()
	if c.closed.Load() {
		return
	}
	c.captured.Add(1)
	if p.Environment == "" {
		p.Environment = c.cfg.Environment
	}
	if p.Service == "" {
		p.Service = c.cfg.ServiceName
	}
	p.Metadata = mergeReleaseTags(p.Metadata, c.cfg.ReleaseTags())

	enqueue(c, c.logs, &p)
}

// CaptureHTTPRequest enqueues an inbound or outbound HTTP event.
func (c *Client) CaptureHTTPRequest(p HTTPRequestItem) {
	defer c.guardCapture()
	if c.closed.Load() {
		return
	}
	c.captured.Add(1)
	if p.Environment == "" {
		p.Environment = c.cfg.Environment
	}
	if p.Release == "" {
		p.Release = c.cfg.Release
	}
	if p.Timestamp == "" {
		p.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	p.Metadata = mergeReleaseTags(p.Metadata, c.cfg.ReleaseTags())

	enqueue(c, c.requests, &p)
}

// CaptureDBQuery enqueues a database query event.
func (c *Client) CaptureDBQuery(p DBQueryItem) {
	defer c.guardCapture()
	if c.closed.Load() {
		return
	}
	c.captured.Add(1)
	if p.Environment == "" {
		p.Environment = c.cfg.Environment
	}
	if p.Service == "" {
		p.Service = c.cfg.ServiceName
	}
	if p.TimestampMillis == 0 {
		p.TimestampMillis = time.Now().UnixMilli()
	}

	enqueue(c, c.dbq, &p)
}

// CaptureSpan enqueues a tracing span for batched ingest.
func (c *Client) CaptureSpan(p SpanItem) {
	defer c.guardCapture()
	if c.closed.Load() {
		return
	}
	c.captured.Add(1)
	if p.Environment == "" {
		p.Environment = c.cfg.Environment
	}
	if p.Service == "" {
		p.Service = c.cfg.ServiceName
	}

	enqueue(c, c.spans, &p)
}

// SendHeartbeat reports a cron/job completion synchronously. Unlike the
// queued capture methods this blocks until the backend responds (or the
// context passed to the parent call is cancelled) because the caller
// usually wants to know whether the ping was accepted.
func (c *Client) SendHeartbeat(ctx context.Context, p HeartbeatPayload) (err error) {
	// Public API boundary guard: convert any internal panic into an error
	// instead of crashing the host.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("allstak: heartbeat panic suppressed: %v", r)
		}
	}()
	if c.closed.Load() {
		return errors.New("allstak: client closed")
	}
	c.captured.Add(1)
	if p.Status == "" {
		p.Status = "success"
	}
	return c.transport.send(ctx, pathHeartbeat, p)
}

// ── Lifecycle ─────────────────────────────────────────────────────────────

// Flush blocks until all currently-queued events have been sent or the
// given context is cancelled. This is primarily useful right before
// program exit and in tests.
func (c *Client) Flush(ctx context.Context) error {
	// We flush by draining each queue to empty. Because the workers read
	// from the same channels, we wait until the channels are empty and
	// in-flight sends have observed that emptiness.
	deadline := time.Now().Add(3 * time.Second)
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}

	for {
		if time.Now().After(deadline) {
			return errors.New("allstak: flush deadline exceeded")
		}
		if len(c.errs) == 0 && len(c.logs) == 0 && len(c.requests) == 0 && len(c.dbq) == 0 && len(c.spans) == 0 {
			// Give workers a moment to finish the send they started when
			// they pulled the last item off the channel.
			time.Sleep(20 * time.Millisecond)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Close flushes any buffered events and stops the background workers.
// It is idempotent — calling Close twice is a no-op.
func (c *Client) Close(ctx context.Context) error {
	var firstErr error
	c.closeOnce.Do(func() {
		// Order matters for the close/send race:
		//  1. Set `closed` so new captures bail at the fast path.
		//  2. Flush so already-queued events are delivered.
		//  3. Take the WRITE lock — this waits for every in-flight enqueue
		//     (each holds the read lock across its closed-check + send) to
		//     finish, and any enqueue that starts afterwards observes
		//     closed==true under the read lock and returns without sending.
		//  4. Only then close the channels. No goroutine can be inside a send.
		c.closed.Store(true)
		if err := c.Flush(ctx); err != nil {
			firstErr = err
		}
		// Release-health: close the session with its final status. Best-effort,
		// short timeout, fail-open — must not block shutdown or surface errors.
		c.endSession()
		c.cancel()
		c.sendMu.Lock()
		// Close channels so workers can exit cleanly.
		close(c.errs)
		close(c.logs)
		close(c.requests)
		close(c.dbq)
		close(c.spans)
		c.sendMu.Unlock()
		c.wg.Wait()
		c.debugf("client closed: sent=%d dropped=%d failed=%d", c.sent.Load(), c.dropped.Load(), c.failed.Load())
	})
	return firstErr
}

// ── Internal helpers ──────────────────────────────────────────────────────

// persistOnFailure writes a failed-to-deliver payload to the persistent spool
// so it survives a restart/outage and can be replayed on the next init. It is
// the worker layer's single persistence hook. No-op (fail-open) when:
//
//   - the spool is disabled/unavailable (nil receiver),
//   - the path is not persistable (sessions/heartbeat/releases are live-only),
//   - the failure was a PERMANENT 4xx — replaying it would never succeed, so
//     it is correctly dropped rather than spooled.
//
// The payload is run through the PII sanitizer again here before it is written,
// so secrets/unredacted data NEVER hit disk even though the transport already
// scrubs on the wire. Scrubbing is idempotent, so the double-scrub is harmless.
func (c *Client) persistOnFailure(path string, payload any, sendErr error, eventCount int64) bool {
	if c.spool == nil || !shouldSpoolPath(path) {
		return false
	}
	if isPermanentSendError(sendErr) {
		// 4xx (non-429): the backend rejected it; persisting would just replay a
		// guaranteed rejection. Drop it (already counted as failed).
		return false
	}
	body, err := marshalScrubbed(payload, c.cfg.scrubOptions())
	if err != nil {
		c.debugf("spool persist marshal failed for %s: %v", path, err)
		return false
	}
	if c.spool.persist(path, body) {
		c.persisted.Add(eventCount)
		return true
	}
	return false
}

func (c *Client) debugf(format string, args ...any) {
	if !c.cfg.Debug {
		return
	}
	fmt.Fprintf(debugWriter(), "[allstak] "+format+"\n", args...)
}
