# Changelog

All notable changes to the AllStak Go SDK are documented in this file. The
format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.3] — 2026-05-18

### Added — canonical denylist parity
Extended `defaultRedactKeyPatterns` in `redaction.go` with the seven terms
that were missing relative to the canonical SDK-platform denylist:
`bearer`, `jwt`, `pwd`, `credit_card` / `creditcard`, `card_number` /
`cardnumber`, `cvv`, `ssn`. Recursion behaviour, the `[REDACTED]`
sentinel, and the `(^|[._-])<term>$` suffix-anchored pattern style are
unchanged. The denylist now matches the same 25-term canonical baseline
used by `@allstak/js`, `@allstak/react-native`, `allstak-python`,
`allstak-ruby`, `AllStak` (.NET), and `allstak_flutter`.

### Live canary E2E
Real on-the-wire proof against `https://api.allstak.sa`:
- Event `c465c398-84a5-468f-9f28-efe411964f4c` (sdk.name=`allstak-go`,
  sdk.version=`0.1.3`, release=`go-canary-10outof10`).
- ClickHouse confirmed `leak_pos = 0` across `metadata`, `stack_trace`,
  `breadcrumbs`, `message`. The canary `should_not_leak_go` was planted
  in `password`, `authorization`, `cookie`, `Bearer`, `api_key`,
  `token`, `jwt`, `bearer`, `pwd`, `credit_card`, `ssn`, `cvv`, and a
  3-level-nested `token`. All scrubbed before the wire.

## [0.1.2] — 2026-05-17

Runtime hardening and privacy pass. Still **beta** — pending live dashboard certification.

### Added
- Header- and metadata-redaction layer. Default deny-list covers `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-API-Key`, `X-Auth-Token`, `X-Access-Token`, `X-AllStak-Key`, and key-suffix patterns `*token`, `*api_key`, `*password`, `*passwd`, `*secret`, `*session_id`, `*csrf`. Applied to:
  - `Config.RedactKeys` extends (never shrinks) the deny-list. Accepts plain substrings or full regexps.
  - `CaptureError.Metadata` and breadcrumb `Data` (recursive into nested maps and slices).
  - `CaptureLog.Metadata` (recursive).
  - `CaptureHTTPRequest.Metadata` (recursive).
  - `CaptureSpan.Tags` (`map[string]string`).
- Optional inbound/outbound header capture: `Config.CaptureRequestHeaders` and `Config.CaptureResponseHeaders` (both **off by default**). When enabled, sensitive header values are written as `[REDACTED]` in `HTTPRequestItem.RequestHeaders` / `.ResponseHeaders`. Bodies remain off and uncaptured.
- Integration tests: `integrations/allstakchi` (3 tests covering inbound capture, panic recovery, header redaction) and `integrations/allstakcron` (5 tests covering success, error, panic-safety, `Wrap` shape, nil-safe init).
- Redaction unit tests covering the default deny-list, custom string/regex patterns, invalid pattern fallthrough, nested-map walks, multi-value header redaction, and capture-path assertions for error/log/request/span metadata.

### Changed
- `sdkVersion` constant bumped to `0.1.2`. Aligned with this CHANGELOG entry and the User-Agent stamp (`allstak-go/0.1.2`).

### Notes
- Published latest at the time of release was `v0.1.1` (no CHANGELOG entry). This `v0.1.2` line is the first release that closes the documented version-drift.
- Live dashboard certification has not yet been recorded; this release does not claim production-stable status.

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
