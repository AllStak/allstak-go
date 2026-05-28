package allstak

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Release-health session endpoints — relative to the resolved host.
const (
	pathSessionsStart = "/ingest/v1/sessions/start"
	pathSessionsEnd   = "/ingest/v1/sessions/end"
)

// Session-status wire values. They match the AllStak backend's
// /ingest/v1/sessions/end contract and Sentry's release-health conventions:
//
//   - "ok"       — session ended normally with at most non-fatal events.
//   - "errored"  — at least one handled error-level event landed during the
//     session, but the process kept running.
//   - "crashed"  — an unhandled/fatal event ended the process (the SDK only
//     reports this when it observes the panic/fatal itself).
//   - "abnormal" — process ended without a normal flush. Reserved for future
//     shutdown telemetry.
const (
	statusOK       = "ok"
	statusErrored  = "errored"
	statusCrashed  = "crashed"
	statusAbnormal = "abnormal"
)

// sessionRank orders the status values so a transition only ever escalates
// severity. A crashed session never downgrades to errored, an errored session
// never downgrades to ok, etc. This mirrors the Java Session model's
// compareAndSet escalation semantics.
func sessionRank(status string) int {
	switch status {
	case statusOK:
		return 0
	case statusErrored:
		return 1
	case statusAbnormal:
		return 2
	case statusCrashed:
		return 3
	default:
		return 0
	}
}

// session is a single release-health session — one per process / app launch
// in the default server-mode deployment. Status transitions are recorded
// in-memory via atomics so any goroutine can mark the session errored/crashed
// without locking; only the terminal end() call performs network I/O.
//
// It mirrors dev.allstak.session.Session in the Java SDK.
type session struct {
	id        string
	startedAt time.Time

	// status holds the current wire status string. Mutated only through
	// escalate() so it never downgrades.
	status atomic.Value // string
	errors atomic.Int64
}

func newSession() *session {
	s := &session{
		id:        newSessionID(),
		startedAt: time.Now(),
	}
	s.status.Store(statusOK)
	return s
}

func (s *session) currentStatus() string {
	if v, ok := s.status.Load().(string); ok {
		return v
	}
	return statusOK
}

// escalate moves the status to next only when next is strictly more severe
// than the current status. Safe for concurrent callers.
func (s *session) escalate(next string) {
	for {
		cur := s.currentStatus()
		if sessionRank(next) <= sessionRank(cur) {
			return
		}
		if s.status.CompareAndSwap(cur, next) {
			return
		}
	}
}

// recordError marks the session errored (a handled error was captured) and
// bumps the error counter. No network I/O. Matches Session.recordError.
func (s *session) recordError() {
	s.errors.Add(1)
	s.escalate(statusErrored)
}

// recordCrash marks the session crashed (an unhandled/fatal event was
// captured) and bumps the error counter. No network I/O — the end-of-session
// POST carries the final status. Matches Session.recordCrash.
func (s *session) recordCrash() {
	s.errors.Add(1)
	s.escalate(statusCrashed)
}

// durationMs is the elapsed time from start to now, floored at 0.
func (s *session) durationMs() int64 {
	d := time.Since(s.startedAt).Milliseconds()
	if d < 0 {
		return 0
	}
	return d
}

// newSessionID returns a 32-character hex session ID (128 bits of entropy).
// It never panics: a read failure falls back to a timestamp-derived value so
// session tracking stays fail-open.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// sessionStartPayload is the body of POST /ingest/v1/sessions/start. Field
// names mirror the backend SessionStartIngestRequest exactly.
type sessionStartPayload struct {
	SessionID   string `json:"sessionId"`
	Release     string `json:"release"`
	Environment string `json:"environment,omitempty"`
	UserID      string `json:"userId,omitempty"`
	SDKName     string `json:"sdkName,omitempty"`
	SDKVersion  string `json:"sdkVersion,omitempty"`
	Platform    string `json:"platform,omitempty"`
}

// sessionEndPayload is the body of POST /ingest/v1/sessions/end.
type sessionEndPayload struct {
	SessionID  string `json:"sessionId"`
	DurationMs int64  `json:"durationMs"`
	Status     string `json:"status,omitempty"`
}

// sessionTracker owns the process's single release-health session. It is
// re-entrancy safe: a second start() is a no-op, and once ended the tracker
// does not re-arm. One instance per Client. Mirrors the Java SessionTracker.
type sessionTracker struct {
	startOnce sync.Once
	endOnce   sync.Once
	active    atomic.Pointer[session]
}

// start arms the session and fires the /sessions/start POST through the
// client's transport on a background goroutine so SDK init never blocks the
// host application on a network round-trip. Idempotent. Fail-open: any
// transport error is swallowed. Sessions are NEVER sampled.
func (c *Client) startSession() {
	if c.session == nil {
		return
	}
	c.session.startOnce.Do(func() {
		s := newSession()
		c.session.active.Store(s)

		// Release falls back to the SDK version so release-health can always
		// attribute the session — applyDefaults already does this, but guard
		// here too in case the tracker is constructed directly in a test.
		release := c.cfg.Release
		if release == "" {
			release = c.cfg.SDKVersion
		}

		payload := sessionStartPayload{
			SessionID:   s.id,
			Release:     release,
			Environment: c.cfg.Environment,
			SDKName:     c.cfg.SDKName,
			SDKVersion:  c.cfg.SDKVersion,
			Platform:    c.cfg.Platform,
		}
		if u := c.cfg.User; u != nil {
			payload.UserID = u.ID
		}

		go func() {
			defer func() { _ = recover() }() // never let a session POST crash boot
			ctx, cancel := context.WithTimeout(context.Background(), c.cfg.RequestTimeout)
			defer cancel()
			if err := c.transport.send(ctx, pathSessionsStart, payload); err != nil {
				c.debugf("session start failed: %v", err)
				return
			}
			c.debugf("session started: %s", s.id)
		}()
	})
}

// recordSessionError marks the active session errored. No-op if tracking is
// disabled or the session never started. No network I/O.
func (c *Client) recordSessionError() {
	if c.session == nil {
		return
	}
	if s := c.session.active.Load(); s != nil {
		s.recordError()
	}
}

// recordSessionCrash marks the active session crashed. No-op if tracking is
// disabled or the session never started. No network I/O.
func (c *Client) recordSessionCrash() {
	if c.session == nil {
		return
	}
	if s := c.session.active.Load(); s != nil {
		s.recordCrash()
	}
}

// sessionID returns the active session's id, or "" if tracking is disabled or
// the session has not started. Used to stamp every error/event payload so the
// backend's error consumer can attribute crash-free rates.
func (c *Client) sessionID() string {
	if c.session == nil {
		return ""
	}
	if s := c.session.active.Load(); s != nil {
		return s.id
	}
	return ""
}

// endSession computes the duration and POSTs /sessions/end with the final
// accumulated status. Best-effort, short timeout, idempotent. Must never
// block for long or throw. Called from Close().
func (c *Client) endSession() {
	if c.session == nil {
		return
	}
	c.session.endOnce.Do(func() {
		s := c.session.active.Swap(nil)
		if s == nil {
			return
		}
		payload := sessionEndPayload{
			SessionID:  s.id,
			DurationMs: s.durationMs(),
			Status:     s.currentStatus(),
		}

		// Short, bounded timeout so shutdown never hangs on a slow backend.
		timeout := c.cfg.RequestTimeout
		if timeout <= 0 || timeout > 2*time.Second {
			timeout = 2 * time.Second
		}
		func() {
			defer func() { _ = recover() }()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			if err := c.transport.send(ctx, pathSessionsEnd, payload); err != nil {
				c.debugf("session end failed: %v", err)
				return
			}
			c.debugf("session ended: %s status=%s errors=%d", s.id, payload.Status, s.errors.Load())
		}()
	})
}

// shouldTrackSessions resolves the enableAutoSessionTracking flag. Default is
// ON. Under the Go test runtime (binary name ends in ".test") tracking is
// skipped unless explicitly enabled, mirroring the Java SDK's test guard so
// the SDK's own and host applications' unit tests don't emit sessions.
func shouldTrackSessions(flag *bool) bool {
	if flag != nil {
		return *flag
	}
	return !strings.HasSuffix(os.Args[0], ".test")
}
