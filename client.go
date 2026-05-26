package allstak

import (
	"context"
	"errors"
	"fmt"
	"os"
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

	// Lifecycle
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    atomic.Bool

	// Counters exposed via Stats() for observability/tests.
	sent    atomic.Int64
	dropped atomic.Int64
	failed  atomic.Int64
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
func New(cfg Config) *Client {
	cfg = cfg.applyDefaults()
	host := resolveHost()
	transport := newHTTPTransport(host, cfg.APIKey, cfg.RequestTimeout, cfg.MaxRetries, cfg.Debug)
	return newWithTransport(cfg, host, transport)
}

// NewWithTransport lets advanced users (and tests) inject a custom
// transport. Most applications should use New.
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
		stderrWriter = os.Stderr
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

	c.debugf("client initialized: host=%s env=%s service=%s release=%s", host, cfg.Environment, cfg.ServiceName, cfg.Release)
	return c
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
	Sent    int64
	Dropped int64
	Failed  int64
}

// Stats returns a point-in-time copy of the SDK's send counters.
func (c *Client) Stats() Stats {
	return Stats{
		Sent:    c.sent.Load(),
		Dropped: c.dropped.Load(),
		Failed:  c.failed.Load(),
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

// CaptureError enqueues an error payload. Safe to call from any goroutine.
// Returns immediately; the actual ingest happens asynchronously.
//
// If the queue is full the oldest buffered error is dropped to make room.
// This preserves the most-recent failure which is usually the most useful
// for debugging.
func (c *Client) CaptureError(p ErrorPayload) {
	if c.closed.Load() {
		return
	}
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

	// Pipeline order: SampleRate drop first (skip dropped events entirely),
	// then BeforeSend (may mutate or drop). The PII sanitizer runs later, at
	// the wire chokepoint in transport.send, so it always sees the final
	// post-BeforeSend payload.
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

	select {
	case c.errs <- &p:
	default:
		// Drop oldest and enqueue — ring-buffer semantics.
		select {
		case <-c.errs:
			c.dropped.Add(1)
		default:
		}
		select {
		case c.errs <- &p:
		default:
			c.dropped.Add(1)
		}
	}
}

// CaptureLog enqueues a structured log event.
func (c *Client) CaptureLog(p LogPayload) {
	if c.closed.Load() {
		return
	}
	if p.Environment == "" {
		p.Environment = c.cfg.Environment
	}
	if p.Service == "" {
		p.Service = c.cfg.ServiceName
	}
	p.Metadata = mergeReleaseTags(p.Metadata, c.cfg.ReleaseTags())

	select {
	case c.logs <- &p:
	default:
		select {
		case <-c.logs:
			c.dropped.Add(1)
		default:
		}
		select {
		case c.logs <- &p:
		default:
			c.dropped.Add(1)
		}
	}
}

// CaptureHTTPRequest enqueues an inbound or outbound HTTP event.
func (c *Client) CaptureHTTPRequest(p HTTPRequestItem) {
	if c.closed.Load() {
		return
	}
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

	select {
	case c.requests <- &p:
	default:
		select {
		case <-c.requests:
			c.dropped.Add(1)
		default:
		}
		select {
		case c.requests <- &p:
		default:
			c.dropped.Add(1)
		}
	}
}

// CaptureDBQuery enqueues a database query event.
func (c *Client) CaptureDBQuery(p DBQueryItem) {
	if c.closed.Load() {
		return
	}
	if p.Environment == "" {
		p.Environment = c.cfg.Environment
	}
	if p.Service == "" {
		p.Service = c.cfg.ServiceName
	}
	if p.TimestampMillis == 0 {
		p.TimestampMillis = time.Now().UnixMilli()
	}

	select {
	case c.dbq <- &p:
	default:
		select {
		case <-c.dbq:
			c.dropped.Add(1)
		default:
		}
		select {
		case c.dbq <- &p:
		default:
			c.dropped.Add(1)
		}
	}
}

// CaptureSpan enqueues a tracing span for batched ingest.
func (c *Client) CaptureSpan(p SpanItem) {
	if c.closed.Load() {
		return
	}
	if p.Environment == "" {
		p.Environment = c.cfg.Environment
	}
	if p.Service == "" {
		p.Service = c.cfg.ServiceName
	}

	select {
	case c.spans <- &p:
	default:
		select {
		case <-c.spans:
			c.dropped.Add(1)
		default:
		}
		select {
		case c.spans <- &p:
		default:
			c.dropped.Add(1)
		}
	}
}

// SendHeartbeat reports a cron/job completion synchronously. Unlike the
// queued capture methods this blocks until the backend responds (or the
// context passed to the parent call is cancelled) because the caller
// usually wants to know whether the ping was accepted.
func (c *Client) SendHeartbeat(ctx context.Context, p HeartbeatPayload) error {
	if c.closed.Load() {
		return errors.New("allstak: client closed")
	}
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
		c.closed.Store(true)
		if err := c.Flush(ctx); err != nil {
			firstErr = err
		}
		c.cancel()
		// Close channels so workers can exit cleanly.
		close(c.errs)
		close(c.logs)
		close(c.requests)
		close(c.dbq)
		close(c.spans)
		c.wg.Wait()
		c.debugf("client closed: sent=%d dropped=%d failed=%d", c.sent.Load(), c.dropped.Load(), c.failed.Load())
	})
	return firstErr
}

// ── Internal helpers ──────────────────────────────────────────────────────

func (c *Client) debugf(format string, args ...any) {
	if !c.cfg.Debug {
		return
	}
	fmt.Fprintf(stderrWriter, "[allstak] "+format+"\n", args...)
}

