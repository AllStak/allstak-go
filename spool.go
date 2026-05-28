package allstak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Offline / persistent event spool.
//
// Goal: buffered telemetry must survive a process restart AND a network
// outage, reaching @sentry parity (Sentry persists envelopes to an offline
// cache dir). When an event cannot be delivered — network error, retries
// exhausted, the host is offline, or the process is shutting down with the
// in-memory queue still full — the (already PII-scrubbed) payload is written
// to a filesystem spool directory instead of being dropped. On the next SDK
// init a background goroutine loads the spooled envelopes and re-sends them
// through the normal transport (which already owns retry/backoff/Retry-After).
//
// Design choices that keep this fail-open and minimal:
//
//   - One file per envelope. No append-log, no compaction, no fsync storms —
//     each file is written atomically (temp + rename) so a crash mid-write can
//     never leave a half-written file that breaks drain.
//   - The envelope stores the destination ingest path and the raw,
//     already-scrubbed JSON body. On drain we re-send the body verbatim via
//     json.RawMessage, so the spool is payload-type-agnostic and the wire bytes
//     are byte-for-byte identical to the original attempt.
//   - The store is bounded by count, total bytes, AND max age. When full the
//     OLDEST envelope is evicted. Filenames are lexically sortable by creation
//     time so "oldest" is just the first name.
//   - EVERYTHING is fail-open. A read-only FS, a serverless sandbox, a missing
//     cache dir, an unreadable file — any of these degrades silently to the
//     existing in-memory behavior. The spool never throws and never blocks
//     init or capture.
//
// Session lifecycle calls (/ingest/v1/sessions/start and /end) are NEVER
// persisted: a replayed stale session would skew release-health durations.
// Only error/log/http/db/span telemetry is spooled (the worker layer is the
// sole caller of persist()).

// Spool defaults. Tuned for a server runtime: a few MB of disk, a few hundred
// envelopes, and a max age long enough to survive an overnight outage but
// short enough that replayed data is still useful.
const (
	defaultSpoolMaxEntries = 500
	defaultSpoolMaxBytes   = 8 << 20 // 8 MiB
	defaultSpoolMaxAge     = 48 * time.Hour
	spoolDirName           = "allstak-spool"
	spoolFileExt           = ".json"
	spoolFilePrefix        = "evt-"
)

// spoolEnvelope is the on-disk record for one un-sent event. Body is the
// already-scrubbed JSON wire payload, stored raw so re-send is byte-identical.
type spoolEnvelope struct {
	// V is the envelope schema version so a future format change can be
	// detected and skipped rather than mis-parsed.
	V int `json:"v"`
	// Path is the ingest endpoint the body was destined for (e.g.
	// "/ingest/v1/errors"). Replayed verbatim on drain.
	Path string `json:"path"`
	// CreatedAt is the unix-millis the envelope was spooled. Used for max-age
	// eviction and to skip stale envelopes on drain.
	CreatedAt int64 `json:"createdAt"`
	// Body is the scrubbed JSON body. Marshalled/unmarshalled as RawMessage so
	// the spool stays agnostic to the concrete payload type.
	Body json.RawMessage `json:"body"`
}

const spoolEnvelopeVersion = 1

// spool is a bounded, filesystem-backed persistent event store. A nil *spool
// is valid and means "disabled / degraded to in-memory" — every method is a
// safe no-op on a nil receiver, so callers never need to nil-check.
type spool struct {
	dir        string
	maxEntries int
	maxBytes   int64
	maxAge     time.Duration

	// mu serializes the read-modify-write of the directory so concurrent
	// worker persist() calls cannot race on eviction. Disk I/O under a single
	// mutex is acceptable here: persist is a rare, off-the-hot-path failure
	// fallback, not a per-event cost.
	mu sync.Mutex

	debugf func(format string, args ...any)
}

// resolveSpoolDir picks the spool directory. Explicit config wins; otherwise
// os.UserCacheDir() (XDG_CACHE_HOME / ~/Library/Caches / %LocalAppData%) with a
// fallback to os.TempDir() for sandboxed runtimes where UserCacheDir fails. The
// returned path is NOT yet created — newSpool does that and decides writability.
func resolveSpoolDir(configured string) string {
	if s := strings.TrimSpace(configured); s != "" {
		return s
	}
	if base, err := os.UserCacheDir(); err == nil && base != "" {
		return filepath.Join(base, spoolDirName)
	}
	return filepath.Join(os.TempDir(), spoolDirName)
}

// newSpool constructs an enabled spool rooted at dir, creating the directory if
// needed and probing for writability. It returns nil — disabling persistence —
// when the directory cannot be created or written, so the SDK degrades silently
// to in-memory on a read-only FS or sandboxed runtime. Never panics.
func newSpool(dir string, maxEntries int, maxBytes int64, maxAge time.Duration, debugf func(string, ...any)) *spool {
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	if maxEntries <= 0 {
		maxEntries = defaultSpoolMaxEntries
	}
	if maxBytes <= 0 {
		maxBytes = defaultSpoolMaxBytes
	}
	if maxAge <= 0 {
		maxAge = defaultSpoolMaxAge
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		debugf("spool disabled: mkdir %s: %v", dir, err)
		return nil
	}
	// Probe writability with a temp file rather than trusting MkdirAll, which
	// succeeds on an existing read-only dir.
	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		debugf("spool disabled: dir %s not writable: %v", dir, err)
		return nil
	}
	probeName := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probeName)

	return &spool{
		dir:        dir,
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		maxAge:     maxAge,
		debugf:     debugf,
	}
}

// persist writes one already-scrubbed envelope to disk, then enforces the
// count/byte bounds by evicting the oldest entries. Fail-open: any error is
// swallowed (logged in debug) so a full or read-only disk never breaks capture
// or shutdown. Safe on a nil receiver.
func (s *spool) persist(path string, body []byte) {
	if s == nil {
		return
	}
	env := spoolEnvelope{
		V:         spoolEnvelopeVersion,
		Path:      path,
		CreatedAt: time.Now().UnixMilli(),
		Body:      json.RawMessage(body),
	}
	data, err := json.Marshal(env)
	if err != nil {
		s.debugf("spool marshal failed: %v", err)
		return
	}
	// A single envelope larger than the whole budget is pointless to store.
	if int64(len(data)) > s.maxBytes {
		s.debugf("spool skip: envelope %d bytes exceeds budget %d", len(data), s.maxBytes)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	name := s.nextName()
	if err := s.atomicWrite(name, data); err != nil {
		s.debugf("spool write failed: %v", err)
		return
	}
	s.enforceBoundsLocked()
}

// seq is a process-local monotonic counter appended to the millis timestamp so
// two envelopes spooled in the same millisecond still sort deterministically
// and never collide on filename.
var spoolSeq atomic.Uint64

// nextName builds a lexically-sortable filename: prefix + zero-padded millis +
// "-" + zero-padded sequence + ext. Lexical order == creation order, so the
// oldest file is always the lexically-first one.
func (s *spool) nextName() string {
	ms := time.Now().UnixMilli()
	seq := spoolSeq.Add(1)
	return fmt.Sprintf("%s%013d-%010d%s", spoolFilePrefix, ms, seq, spoolFileExt)
}

// atomicWrite writes data to a temp file in the spool dir then renames it into
// place, so a crash mid-write can never leave a partial envelope that breaks
// drain. rename within the same dir is atomic on POSIX and Windows.
func (s *spool) atomicWrite(name string, data []byte) error {
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(s.dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// listEnvelopesLocked returns the spool envelope filenames sorted oldest-first.
// Hidden temp/probe files are ignored.
func (s *spool) listEnvelopesLocked() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, spoolFilePrefix) && strings.HasSuffix(n, spoolFileExt) {
			names = append(names, n)
		}
	}
	sort.Strings(names) // lexical == oldest-first by construction
	return names
}

// enforceBoundsLocked evicts oldest envelopes until the store is within the
// count and byte caps. Called under mu after every write.
func (s *spool) enforceBoundsLocked() {
	names := s.listEnvelopesLocked()

	// Total size.
	var total int64
	sizes := make(map[string]int64, len(names))
	for _, n := range names {
		if fi, err := os.Stat(filepath.Join(s.dir, n)); err == nil {
			sizes[n] = fi.Size()
			total += fi.Size()
		}
	}

	// Drop oldest until both bounds hold. names is oldest-first.
	i := 0
	for len(names)-i > s.maxEntries || total > s.maxBytes {
		if i >= len(names) {
			break
		}
		victim := names[i]
		if err := os.Remove(filepath.Join(s.dir, victim)); err == nil {
			total -= sizes[victim]
			s.debugf("spool evicted oldest: %s", victim)
		}
		i++
	}
}

// drain loads every spooled envelope oldest-first and re-sends it through the
// transport, honoring the transport's own retry/backoff/Retry-After. An
// envelope is removed only after it is accepted (send returns nil → 2xx) or is
// permanently undeliverable (a 4xx other than 429, surfaced by the transport as
// a permanentSendError). Transient failures (network/5xx/429/retries-exhausted)
// leave the envelope on disk for a future drain. Stale envelopes past maxAge are
// dropped without sending. Fail-open and respects ctx cancellation. Safe on a
// nil receiver.
func (s *spool) drain(ctx context.Context, t ingestTransport) {
	if s == nil || t == nil {
		return
	}
	s.mu.Lock()
	names := s.listEnvelopesLocked()
	s.mu.Unlock()

	cutoff := time.Now().Add(-s.maxAge).UnixMilli()

	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		full := filepath.Join(s.dir, name)
		raw, err := os.ReadFile(full)
		if err != nil {
			continue // someone else may have drained/evicted it
		}
		var env spoolEnvelope
		if err := json.Unmarshal(raw, &env); err != nil || env.V != spoolEnvelopeVersion || env.Path == "" {
			// Corrupt or unknown-version envelope — drop it so it can't wedge
			// the drain loop forever.
			_ = os.Remove(full)
			continue
		}
		if env.CreatedAt > 0 && env.CreatedAt < cutoff {
			s.debugf("spool drop stale: %s", name)
			_ = os.Remove(full)
			continue
		}

		// Re-send the scrubbed body verbatim. RawMessage marshals to itself, so
		// the transport puts the exact original bytes back on the wire. The body
		// was already scrubbed before it was persisted, so no re-scrub is needed
		// (and json.RawMessage is opaque to the scrubber anyway).
		err = t.send(ctx, env.Path, env.Body)
		if err == nil {
			_ = os.Remove(full) // accepted (2xx)
			continue
		}
		if isPermanentSendError(err) {
			s.debugf("spool drop permanently-undeliverable %s: %v", name, err)
			_ = os.Remove(full) // 4xx (non-429) — never going to succeed
			continue
		}
		// Transient — leave it on disk for the next drain.
		s.debugf("spool keep (transient) %s: %v", name, err)
	}
}

// count returns the number of spooled envelopes. Test/observability helper.
// Returns 0 on a nil receiver or unreadable dir.
func (s *spool) count() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.listEnvelopesLocked())
}

// shouldSpoolPath reports whether telemetry for the given ingest path may be
// persisted. Session lifecycle calls are explicitly excluded: a replayed stale
// session would skew release-health durations. Heartbeats and releases are
// likewise live-only and not spooled. Only error/log/http/db/span telemetry is
// persistable.
func shouldSpoolPath(path string) bool {
	switch path {
	case pathErrors, pathLogs, pathHTTPRequests, pathDBQueries, pathSpans:
		return true
	default:
		// pathSessionsStart, pathSessionsEnd, pathHeartbeat, pathReleases.
		return false
	}
}

// isPermanentSendError reports whether err is a permanent (4xx non-429) ingest
// rejection. The transport wraps such failures in permanentSendError; a nil err
// (success) is not permanent.
func isPermanentSendError(err error) bool {
	if err == nil {
		return false
	}
	var p *permanentSendError
	return errors.As(err, &p)
}

// permanentSendError marks an ingest rejection the transport must NOT retry and
// the spool must drop rather than keep: a 4xx status other than 429. It carries
// the status code for debugging. Transient failures (network, 5xx, 429, retries
// exhausted) are returned as plain errors instead.
type permanentSendError struct {
	statusCode int
	path       string
	body       string
}

func (e *permanentSendError) Error() string {
	return fmt.Sprintf("allstak: ingest %s returned %d: %s", e.path, e.statusCode, truncate(e.body, 300))
}

// is4xxPermanent reports whether status is a 4xx that should be treated as
// permanent (everything except 429 Too Many Requests).
func is4xxPermanent(status int) bool {
	return status >= 400 && status < 500 && status != http.StatusTooManyRequests
}
