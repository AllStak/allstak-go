# Changelog

All notable changes to the AllStak Go SDK are documented in this file. The
format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.0] — 2026-05-29

### Added

- **Release-health session tracking.** The SDK now opens one release-health
  session per process on `New` (`POST /ingest/v1/sessions/start`), tracks an
  in-memory `ok` → `errored` → `crashed` status that only ever escalates
  severity, and posts the terminal status with a duration on `Close`
  (`POST /ingest/v1/sessions/end`), enabling crash-free-session/-user rates on
  the dashboard. Status moves to `errored` when a handled error is captured and
  `crashed` on an unhandled/fatal event. Every error/event payload is stamped
  with the active `sessionId` for attribution. Sessions are NEVER sampled and
  NEVER spooled (a replayed stale session would skew release-health durations).
  New config:
  - **`Config.EnableAutoSessionTracking`** (`*bool`, default ON). Go test
    binaries are skipped unless explicitly set to `true` so unit tests do not
    emit sessions. Set to a pointer to `false` to opt out.
  - **`Config.User`** (`*UserContext`) — optional process-level principal whose
    `id` is stamped on the session-start payload for crash-free-user rates.
  - **`Config.Platform`** (`string`, default `"go"`) — runtime identifier sent
    on session start and as the wire `platform` field.
- **Offline / persistent transport queue.** Telemetry that cannot be delivered
  (network outage, retries exhausted, or events still buffered at shutdown) is
  written — already PII-scrubbed — to a bounded filesystem spool and replayed
  through the normal transport on the next init, so buffered telemetry survives
  a process restart and a network outage (offline-cache parity). One file
  per envelope, atomic temp+rename writes, bounded by count, total bytes, and
  max age with oldest-first eviction. Fail-open: degrades silently to in-memory
  on a read-only FS / serverless / edge runtime. Sessions are never spooled. New
  config:
  - **`Config.EnableOfflineQueue`** (`*bool`, default ON; pointer to `false` to
    disable).
  - **`Config.OfflineQueueDir`** (default `os.UserCacheDir()/allstak-spool`,
    falling back to `os.TempDir()/allstak-spool`).
  - **`Config.OfflineQueueMaxEntries`** (default 500),
    **`Config.OfflineQueueMaxBytes`** (default 8 MiB),
    **`Config.OfflineQueueMaxAge`** (default 48h).
- **`Config.BeforeSend`** hook (`func(event *ErrorPayload) *ErrorPayload`).
  Invoked once per error/message event, just before it is enqueued to the
  transport — after the `SampleRate` gate and before the PII sanitizer.
  Mutate-and-return to modify the event, or return `nil` to drop it. Fail-open:
  a panicking callback is recovered and the original event is sent rather than
  crashing capture.
- **`Config.SampleRate`** (`float64`, default 1.0). Deterministic random drop
  of error/message events at capture time. A zero/unset value means keep
  everything (1.0); out-of-range values are clamped to `[0,1]`.
- **`Config.TracesSampleRate`** (`float64`, default 0 = disabled). Samples span
  creation in `StartSpan`. A child span inherits its parent trace's decision;
  root spans draw from this rate. The decision is reflected in the propagated
  W3C `traceparent` trace-flags (`-01` sampled, `-00` not sampled), replacing
  the previously hardcoded `-01`. A zero/unset value records every span the
  caller explicitly starts.
- **Automatic runtime release detection.** When `Release` is unset the SDK
  derives it from the binary's embedded VCS info (`runtime/debug.ReadBuildInfo`
  `vcs.revision`), so a release is attributed CI-free, falling back to the SDK
  version.
- **`integrations/allstakecho`** (nested module) — Echo v4 middleware mirroring
  the net/http behavior: inbound HTTP capture, panic→fatal recovery, returned-
  error capture, and trace-context propagation with the matched route template.

### Security

- **Value-pattern PII scrubbing + `Config.SendDefaultPii`.** Beyond the existing
  key-name secret denylist, free-text values (error/log/breadcrumb messages,
  metadata/extra/tag values, captured headers/bodies) are now scrubbed by
  pattern. Luhn-valid credit-card numbers and hyphenated US SSNs are ALWAYS
  redacted. Email addresses and IPv4 addresses are scrubbed by default and pass
  through only when `Config.SendDefaultPii` is set to a pointer to `true`
  (mirrors the `send_default_pii`, default FALSE). The
  `ALLSTAK_SEND_DEFAULT_PII` env var can toggle it without a code change. The
  explicit `WithUser` user object (`id`/`email`/`ip`) is intentional
  identification and is unaffected, matching.

### Fixed

- **Integration module path casing.** Nested integration modules now import the
  root module as `github.com/AllStak/allstak-go` (correct org casing), keeping
  `go.mod` paths consistent with the published module.
- **Data race in the Gin and GORM integration tests.** The test transport
  callback runs in the SDK background batch-worker goroutine; the captured
  result slices are now guarded by a `sync.Mutex` so `go test -race ./...` is
  clean in those submodules (matching the existing Echo test pattern).

## [0.2.0] — 2026-05-13

Module path migration. The published module path moved from `allstak-io` to
`github.com/AllStak/allstak-go`. The wire format and public API are unchanged
from 0.1.0; this is a re-home of the canonical import path.

### Changed

- Module path: `allstak-io/...` → `github.com/AllStak/allstak-go`.
- `sdk.version` and the `User-Agent` header now report `0.2.0`, matching the
  released tag. (Previously the wire `sdk.version` reported `0.2.0` while the
  `User-Agent` still reported `0.1.0`; both now derive from a single
  `SDKVersion` constant.)

### Security

- **Outbound PII / secret scrubbing.** Event metadata, log fields, breadcrumb
  data, and span tags are now recursively scrubbed against a case-insensitive
  key denylist (password, token, authorization, cookie, session, jwt, ssn,
  credit-card, etc.) at the single transport chokepoint before marshalling.
  Matched values are replaced with `[REDACTED]`. Scrubbing is fail-open and
  bounded (depth + cycle guards), mirroring the Python/Java SDKs.

### Fixed

- `Retry-After` is now honored on `429` and `503` responses (integer-seconds
  or HTTP-date), clamped to 300s, falling back to exponential backoff when the
  header is absent or unparseable.

## [0.1.0] — 2026-04-11

Initial public release of the Go SDK. v0.1.0 is considered **stable for
production ingest** — the wire format is locked to the Java/PHP/JS/Python
SDK contracts and will not break within the 0.x line. The public API
surface may still evolve; changes will be called out here.

### Highlights

- **One-line middleware for net/http and Chi** — automatic inbound HTTP
  capture, panic recovery, and trace context propagation.
- **Gin, GORM, and cron integrations** — shipped as opt-in nested modules so
  you only pull in framework deps you actually use.
- **Outbound HTTP capture** via a drop-in `http.RoundTripper` wrapper.
- **Distributed trace propagation** via `X-AllStak-Trace-Id` and
  `X-AllStak-Span-Id` headers on both inbound and outbound requests.
- **Mutable request-state bag** so late-arriving user info from downstream
  auth middleware is visible to the outer panic handler — this fixes a
  common middleware-ordering gotcha that trips up every other language SDK.

### Added

- `allstak.Client` — thread-safe client with one background worker per
  stream (errors, logs, requests, db, spans), bounded ring-buffer queues
  with ring-buffer drop semantics when full, exponential backoff + jitter
  on retries, graceful `Flush` and `Close`.
- `allstak.Config` — compact public config surface (`APIKey`,
  `Environment`, `Release`, `ServiceName`, `Debug`, plus tuning knobs).
- `INGEST_HOST` static constant with opt-in `ALLSTAK_HOST` env override
  for self-hosted / local development. No `Host` field on `Config`.
- `CaptureException`, `CaptureExceptionWithLevel`, `CaptureMessage`,
  `CaptureError` — high-level and low-level error capture.
- `Recover` (re-panics) and `RecoverAndSuppress` (swallows) deferred
  helpers for panic capture in goroutines.
- `CapturePanicValue` — integration-facing wrapper for framework
  middlewares that do their own `recover()`.
- `Info`, `Warn`, `Error`, `Debug` — structured log helpers with
  `Field` key-value pairs.
- `StartSpan` — manual span boundary with `finish(err)` callback.
- `Middleware` — net/http middleware (inbound HTTP, panic recovery,
  trace context, user enrichment via shared state bag).
- `NewTransport` — `http.RoundTripper` wrapper for outbound capture.
- `WithUser`, `UserFromContext`, `WithRequestID`, `RequestIDFromContext`,
  `WithRequestInfo`, `RequestInfoFromContext`, `TraceFromContext`,
  `SpanFromContext`, `WithContextSpan`, `WithRequestState` — context
  helpers for integrations and user code.
- `NewTraceID`, `NewSpanID` — 128-bit / 64-bit hex ID generators.
- `NormalizeSQL`, `HashSQL`, `ClassifySQL` — SQL normalization used by the
  GORM integration and exported for custom DB wrappers.
- `SendHeartbeat` — synchronous cron heartbeat ping.
- `Stats` — snapshot of sent/dropped/failed counters.
- **`integrations/allstakchi`** — thin re-export of `Middleware` for Chi
  routers; stdlib-only, no extra deps.
- **`integrations/allstakgin`** (nested module) — Gin `HandlerFunc`
  middleware mirroring the net/http behavior.
- **`integrations/allstakgorm`** (nested module) — GORM plugin wiring
  before/after callbacks on Create / Query / Update / Delete / Row / Raw
  with automatic trace correlation via `db.WithContext(ctx)`.
- **`integrations/allstakcron`** — `Wrap`/`RunJob` helpers compatible with
  `robfig/cron/v3` (no import dependency on cron). Captures heartbeat,
  span, and any error the job returns or panics with.

### Validation

Validated end-to-end against a real Chi + GORM + SQLite + JWT tasks app
(`allstak-go-validation`). Verified dashboard rendering for:

- Errors (warn/error/fatal grouping, user context, trace IDs)
- Logs (info/warn/error, trace + user correlation)
- Requests (inbound + outbound, status codes, durations, users)
- Database (INSERT / UPDATE / DELETE / SELECT / CREATE, normalized SQL,
  durations, status, rows affected)
- Traces (trace ID propagation from inbound → DB → outbound)
- Cron monitors (healthy + failed states with auto-create on first ping)

See `allstak-go-validation/` in the AllStak workspace for the reference
integration.

### Fixed during validation

- User context now propagates from downstream auth middleware to the
  outer panic handler. Previously, a panic that occurred after the auth
  middleware ran was captured without a user because Go's `context` is
  immutable and the outer middleware held the original pre-auth ctx. The
  fix introduces a pointer-backed `requestState` installed at the top
  of the middleware that `WithUser` mutates rather than creating a new
  child context.
