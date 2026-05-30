package allstak

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
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

const (
	sessionStateVersion     = 1
	sessionStateMaxAge      = 7 * 24 * time.Hour
	sessionRecoveryLockTTL  = 30 * time.Second
	sessionRecoveryMaxTries = 3
	sessionStateDirectory   = "allstak-session-state"
	sessionStateFilePrefix  = "session-"
	sessionStateFileSuffix  = ".json"
)

// Session-status wire values. They match the AllStak backend's
// /ingest/v1/sessions/end contract and standard release-health conventions:
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

type persistedSessionState struct {
	Version           int    `json:"version"`
	SessionID         string `json:"sessionId"`
	StartedAtUnixMs   int64  `json:"startedAt"`
	UpdatedAtUnixMs   int64  `json:"updatedAt"`
	Status            string `json:"status"`
	Release           string `json:"release,omitempty"`
	Environment       string `json:"environment,omitempty"`
	UserID            string `json:"userId,omitempty"`
	SDKName           string `json:"sdkName,omitempty"`
	SDKVersion        string `json:"sdkVersion,omitempty"`
	Platform          string `json:"platform,omitempty"`
	Closed            bool   `json:"closed,omitempty"`
	EndedAtUnixMs     int64  `json:"endedAt,omitempty"`
	RecoveryAttempts  int    `json:"recoveryAttempts,omitempty"`
	RecoveryLockOwner string `json:"recoveryLockOwner,omitempty"`
	RecoveryLockUntil int64  `json:"recoveryLockUntil,omitempty"`
	RecoveredAtUnixMs int64  `json:"recoveredAt,omitempty"`
}

// sessionTracker owns the process's single release-health session. It is
// re-entrancy safe: a second start() is a no-op, and once ended the tracker
// does not re-arm. One instance per Client. Mirrors the Java SessionTracker.
type sessionTracker struct {
	startOnce sync.Once
	endOnce   sync.Once
	active    atomic.Pointer[session]
	statePath string
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
		c.recoverPreviousSession()
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
		c.writeSessionState(persistedSessionState{
			Version:         sessionStateVersion,
			SessionID:       s.id,
			StartedAtUnixMs: s.startedAt.UnixMilli(),
			UpdatedAtUnixMs: time.Now().UnixMilli(),
			Status:          s.currentStatus(),
			Release:         release,
			Environment:     c.cfg.Environment,
			UserID:          payload.UserID,
			SDKName:         c.cfg.SDKName,
			SDKVersion:      c.cfg.SDKVersion,
			Platform:        c.cfg.Platform,
		})

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
		c.updateOpenSessionState(s)
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
		c.updateOpenSessionState(s)
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
		c.writeSessionState(persistedSessionState{
			Version:         sessionStateVersion,
			SessionID:       s.id,
			StartedAtUnixMs: s.startedAt.UnixMilli(),
			UpdatedAtUnixMs: time.Now().UnixMilli(),
			Status:          payload.Status,
			Release:         c.cfg.Release,
			Environment:     c.cfg.Environment,
			SDKName:         c.cfg.SDKName,
			SDKVersion:      c.cfg.SDKVersion,
			Platform:        c.cfg.Platform,
			Closed:          true,
			EndedAtUnixMs:   time.Now().UnixMilli(),
		})

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

func (c *Client) recoverPreviousSession() {
	if c.session == nil {
		return
	}
	st, ok := c.readSessionState()
	if !ok {
		return
	}
	now := time.Now()
	if st.Closed {
		c.removeSessionState()
		return
	}
	startedAt := time.UnixMilli(st.StartedAtUnixMs)
	if st.StartedAtUnixMs <= 0 || now.Sub(startedAt) > sessionStateMaxAge {
		c.removeSessionState()
		return
	}
	if st.RecoveryAttempts >= sessionRecoveryMaxTries {
		c.removeSessionState()
		return
	}
	if st.RecoveryLockUntil > now.UnixMilli() {
		return
	}

	owner := newSessionID()
	st.RecoveryAttempts++
	st.RecoveryLockOwner = owner
	st.RecoveryLockUntil = now.Add(sessionRecoveryLockTTL).UnixMilli()
	st.UpdatedAtUnixMs = now.UnixMilli()
	c.writeSessionState(st)
	claimed, ok := c.readSessionState()
	if !ok || claimed.RecoveryLockOwner != owner {
		return
	}

	status := statusAbnormal
	if st.Status == statusCrashed {
		status = statusCrashed
	}
	payload := sessionEndPayload{
		SessionID:  st.SessionID,
		DurationMs: maxInt64(0, st.UpdatedAtUnixMs-st.StartedAtUnixMs),
		Status:     status,
	}
	timeout := c.cfg.RequestTimeout
	if timeout <= 0 || timeout > 2*time.Second {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := c.transport.send(ctx, pathSessionsEnd, payload); err != nil {
		st.RecoveryLockUntil = 0
		c.writeSessionState(st)
		c.debugf("session recovery failed: %v", err)
		return
	}
	st.Status = status
	st.Closed = true
	st.EndedAtUnixMs = time.Now().UnixMilli()
	st.RecoveredAtUnixMs = st.EndedAtUnixMs
	st.RecoveryLockUntil = 0
	c.writeSessionState(st)
}

func (c *Client) updateOpenSessionState(s *session) {
	st, ok := c.readSessionState()
	if !ok || st.SessionID != s.id || st.Closed {
		return
	}
	st.Status = s.currentStatus()
	st.UpdatedAtUnixMs = time.Now().UnixMilli()
	if u := c.cfg.User; u != nil {
		st.UserID = u.ID
	}
	c.writeSessionState(st)
}

func (c *Client) readSessionState() (persistedSessionState, bool) {
	if c.session == nil || c.session.statePath == "" {
		return persistedSessionState{}, false
	}
	data, err := os.ReadFile(c.session.statePath)
	if err != nil {
		return persistedSessionState{}, false
	}
	var st persistedSessionState
	if err := json.Unmarshal(data, &st); err != nil || !validSessionState(st) {
		c.removeSessionState()
		return persistedSessionState{}, false
	}
	return st, true
}

func (c *Client) writeSessionState(st persistedSessionState) {
	if c.session == nil || c.session.statePath == "" {
		return
	}
	defer func() { _ = recover() }()
	if err := os.MkdirAll(filepath.Dir(c.session.statePath), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := c.session.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, c.session.statePath); err != nil {
		_ = os.Remove(tmp)
	}
}

func (c *Client) removeSessionState() {
	if c.session == nil || c.session.statePath == "" {
		return
	}
	_ = os.Remove(c.session.statePath)
}

func validSessionState(st persistedSessionState) bool {
	if st.Version != sessionStateVersion || st.SessionID == "" || st.StartedAtUnixMs <= 0 || st.UpdatedAtUnixMs <= 0 {
		return false
	}
	return st.Status == statusOK || st.Status == statusErrored || st.Status == statusCrashed || st.Status == statusAbnormal
}

func sessionStatePath(cfg Config) string {
	if strings.HasSuffix(os.Args[0], ".test") && cfg.OfflineQueueDir == "" {
		return ""
	}
	base := cfg.OfflineQueueDir
	if base == "" {
		base = filepath.Join(os.TempDir(), sessionStateDirectory)
	}
	sum := sha256.Sum256([]byte(cfg.APIKey + "|" + cfg.Release + "|" + cfg.Environment))
	return filepath.Join(base, sessionStateFilePrefix+hex.EncodeToString(sum[:8])+sessionStateFileSuffix)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
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
