package allstak

import (
	"os"
	"strings"
	"time"
)

// INGEST_HOST is the production ingest endpoint. Customers should not need to
// know this value — the SDK defaults to it and never exposes a Host knob on
// Config. For self-hosted or local-dev use, set ALLSTAK_HOST in the
// environment; tests may inject a transport directly via NewWithTransport.
const INGEST_HOST = "https://api.allstak.sa"

// envHostOverride lets local validation and tests target a different host
// (e.g. http://localhost:8080) without leaking a Host field into the public
// Config. It is read once at client construction.
const envHostOverride = "ALLSTAK_HOST"

// Config is the public configuration surface. It intentionally stays small:
// adding fields is a breaking API change.
type Config struct {
	// APIKey is the project-scoped ingest key (format: ask_live_xxx...).
	// Required. An empty key puts the client into no-op mode.
	APIKey string

	// Environment is a free-form tag such as "production", "staging", "dev".
	// Defaults to "production".
	Environment string

	// Release identifies the application build, typically a git SHA or
	// semantic version. Optional but strongly recommended.
	Release string

	// ServiceName identifies the application/service in the dashboard.
	// Defaults to the binary name.
	ServiceName string

	// Debug enables verbose internal logging to stderr. Off by default.
	Debug bool

	// BeforeSend is an optional hook invoked once per error/message event,
	// just before it is enqueued to the transport (after the SampleRate gate
	// and after a first PII sanitization pass). It receives the fully-enriched,
	// sanitized ErrorPayload and may mutate it and return it, or return nil to
	// DROP the event. The transport sanitizes the returned event again before
	// persistence/network send, so hooks cannot reintroduce secrets.
	//
	// The callback runs on the calling goroutine. It is FAIL-OPEN: if it
	// panics, the SDK recovers and sends the sanitized pre-callback event
	// rather than crashing the caller. Keep it fast and side-effect-free.
	BeforeSend func(event *ErrorPayload) *ErrorPayload

	// SampleRate is the fraction of error/message events to keep, in [0.0,
	// 1.0]. The decision is a deterministic random draw made at capture time.
	//
	// Zero value: a zero (or unset) SampleRate is treated as 1.0 — keep every
	// event. This matches how the rest of Config treats its zero values and
	// means importing the SDK never silently drops errors. Set e.g. 0.25 to
	// keep ~25% of events. Values outside [0,1] are clamped.
	SampleRate float64

	// TracesSampleRate is the fraction of spans to record, in [0.0, 1.0].
	// Sampling is applied when StartSpan is called, and the decision is
	// reflected in the propagated W3C traceparent sampled flag (-01 sampled,
	// -00 not sampled).
	//
	// Zero value: a zero (or unset) TracesSampleRate means tracing sampling is
	// disabled — every span the caller explicitly starts is recorded. Spans
	// are already opt-in, so the safe default is to keep what was asked for.
	// Set e.g. 0.1 to record ~10% of spans. Values outside [0,1] are clamped.
	TracesSampleRate float64

	// FlushInterval is how often the background worker drains the queue.
	// Defaults to 2s. Must be > 0.
	FlushInterval time.Duration

	// BatchSize is the max number of events per ingest request. Defaults to
	// 50. Batches are flushed as soon as they reach this size.
	BatchSize int

	// QueueCapacity is the maximum number of buffered events per channel
	// before the oldest events start being dropped. Defaults to 1000.
	QueueCapacity int

	// MaxRetries is how many times the transport retries a failed flush
	// with exponential backoff + jitter. Defaults to 3.
	MaxRetries int

	// RequestTimeout is the per-request timeout for each ingest call.
	// Defaults to 5s.
	RequestTimeout time.Duration

	// Dist is the optional build distribution tag (e.g. "linux-amd64").
	// Used to disambiguate multi-arch / multi-platform builds within one
	// release. Optional.
	Dist string

	// CommitSha is the git commit SHA the running binary was built from.
	// Auto-detected from $GIT_COMMIT / $VERCEL_GIT_COMMIT_SHA / $RAILWAY_GIT_COMMIT_SHA
	// when unset. Optional.
	CommitSha string

	// Branch is the git branch the running binary was built from.
	// Auto-detected from $GIT_BRANCH / $VERCEL_GIT_COMMIT_REF when unset.
	// Optional.
	Branch string

	// Platform identifies the runtime — defaults to "go". Override only
	// when embedding the SDK in a hybrid runtime (e.g. cgo binding).
	Platform string

	// SDKName / SDKVersion are sent on the wire as sdk.name / sdk.version.
	// Defaulted from the package constants below; override only for tests.
	SDKName    string
	SDKVersion string

	// AutoDetectRelease gates automatic (CI-free) release detection and the
	// version fallback. When nil (the default) detection is ON: if Release is
	// still empty after explicit + env-var resolution, the SDK reads the VCS
	// revision the Go toolchain stamps at `go build` time, falls back to a
	// guarded `git describe` when the binary carries no VCS info, and finally
	// to SDKVersion so Release is never empty. Set to a pointer to false to
	// opt out and leave Release empty when no explicit/env value was provided.
	AutoDetectRelease *bool

	// AutoRegisterRelease gates best-effort runtime release registration.
	// When nil (the default) registration is ON for normal binaries: once the
	// release is resolved, the SDK posts it to /ingest/v1/releases without
	// requiring CI/CD integration. Go test binaries are skipped unless this is
	// explicitly set to true.
	AutoRegisterRelease *bool

	// EnableAutoSessionTracking gates release-health session tracking. When
	// nil (the default) tracking is ON for normal binaries: the SDK opens one
	// session per process on New and posts /ingest/v1/sessions/start, tracks an
	// in-memory ok/errored/crashed status, and posts /ingest/v1/sessions/end on
	// Close. Go test binaries are skipped unless this is explicitly set to true,
	// so the SDK's own and host applications' unit tests don't emit sessions.
	// Set a pointer to false to opt out entirely.
	EnableAutoSessionTracking *bool

	// User is the optional process-level principal stamped on the session
	// start payload (userId) when set. Leave nil for server-mode services that
	// have no single user. Per-event user attribution still comes from the
	// request context via WithUser.
	User *UserContext

	// EnableOfflineQueue gates the persistent (offline) event spool. When nil
	// (the default) the spool is ON: telemetry that cannot be delivered (network
	// outage, retries exhausted, or events still buffered at shutdown) is written
	// — already PII-scrubbed — to a filesystem cache dir and replayed on the next
	// init, so buffered telemetry survives a process restart and a network
	// outage. Sessions are never spooled. The spool degrades silently to in-memory
	// if the cache dir is unavailable/unwritable (read-only FS, serverless, edge).
	// Set a pointer to false to disable persistence entirely.
	EnableOfflineQueue *bool

	// OfflineQueueDir overrides the spool directory. When empty the SDK uses
	// os.UserCacheDir()/allstak-spool, falling back to os.TempDir()/allstak-spool
	// when no user cache dir is resolvable. Ignored when EnableOfflineQueue is
	// false.
	OfflineQueueDir string

	// OfflineQueueMaxEntries caps the number of persisted envelopes. When the cap
	// is reached the OLDEST envelope is evicted. Zero uses the default (500).
	OfflineQueueMaxEntries int

	// OfflineQueueMaxBytes caps the total on-disk size of persisted envelopes in
	// bytes. When the cap is reached the OLDEST envelopes are evicted. Zero uses
	// the default (8 MiB).
	OfflineQueueMaxBytes int64

	// OfflineQueueMaxAge is the maximum age of a persisted envelope. Envelopes
	// older than this are dropped on drain rather than replayed (a stale event is
	// usually noise). Zero uses the default (48h).
	OfflineQueueMaxAge time.Duration

	// SendDefaultPii controls whether commonly-PII free-text values are sent.
	// It defaults to FALSE (nil = false):
	//
	//   - When FALSE (the default), the SDK additionally scrubs email addresses
	//     and IPv4 addresses out of free-text values (error/log/breadcrumb
	//     messages, metadata/extra/tag values, captured HTTP headers/bodies)
	//     before they go on the wire.
	//   - When TRUE, the host has explicitly opted into PII, so those email/IPv4
	//     value scrubbers are disabled and such values pass through.
	//
	// Independently of this flag, credit-card numbers (Luhn-valid) and
	// hyphenated US SSNs are ALWAYS scrubbed from free-text values, and the
	// existing key-name secret redaction (password/token/cookie/etc.) ALWAYS
	// applies. This flag does NOT affect the explicit User set via WithUser —
	// user.id/email/ip are intentional identification and ship as before.
	//
	// Set a pointer to true to enable; leave nil for the secure default.
	SendDefaultPii *bool
}

// SDK identity sent on the wire as `sdk.name` / `sdk.version`.
const (
	SDKName    = "allstak-go"
	SDKVersion = "0.4.1"
)

// envFirstNonEmpty returns the first non-empty value of the listed env vars,
// or "". Used for release-metadata auto-detection.
func envFirstNonEmpty(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// ReleaseTags returns the release-metadata key/value pairs the SDK attaches
// to every event's `metadata` map. Backend reads these as first-class fields
// once dedicated columns land; in the meantime they ride inside metadata.
func (c Config) ReleaseTags() map[string]string {
	out := map[string]string{}
	if c.SDKName != "" {
		out["sdk.name"] = c.SDKName
	}
	if c.SDKVersion != "" {
		out["sdk.version"] = c.SDKVersion
	}
	if c.Platform != "" {
		out["platform"] = c.Platform
	}
	if c.Dist != "" {
		out["dist"] = c.Dist
	}
	if c.CommitSha != "" {
		out["commit.sha"] = c.CommitSha
	}
	if c.Branch != "" {
		out["commit.branch"] = c.Branch
	}
	return out
}

// applyDefaults fills unset fields with sane defaults and returns a copy.
// This is called exactly once per client construction so customers never see
// mutated values. It never reaches into the host environment for anything
// other than the opt-in ALLSTAK_HOST override, which is read in resolveHost.
func (c Config) applyDefaults() Config {
	if c.Environment == "" {
		c.Environment = "production"
	}
	if c.ServiceName == "" {
		c.ServiceName = defaultServiceName()
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 2 * time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 50
	}
	if c.QueueCapacity <= 0 {
		c.QueueCapacity = 1000
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 3
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 5 * time.Second
	}
	// Release-tracking auto-detection. Explicit values always win.
	if c.Platform == "" {
		c.Platform = "go"
	}
	if c.SDKName == "" {
		c.SDKName = SDKName
	}
	if c.SDKVersion == "" {
		c.SDKVersion = SDKVersion
	}
	// Release resolution, highest precedence first:
	//   1. explicit c.Release (handled by the `== ""` guards — never overwritten)
	//   2. conventional CI env vars
	//   3. automatic VCS detection (build info → guarded git describe)
	//   4. SDKVersion fallback so Release is never empty
	// Steps 3+4 are gated by AutoDetectRelease (nil/true = on).
	if c.Release == "" {
		c.Release = envFirstNonEmpty("ALLSTAK_RELEASE", "VERCEL_GIT_COMMIT_SHA", "RAILWAY_GIT_COMMIT_SHA", "RENDER_GIT_COMMIT")
	}
	if c.Release == "" && (c.AutoDetectRelease == nil || *c.AutoDetectRelease) {
		if rel := resolveAutoRelease(readBuildVCSInfo, defaultGitRunner); rel != "" {
			c.Release = rel
		} else {
			c.Release = c.SDKVersion
		}
	}
	if c.CommitSha == "" {
		c.CommitSha = envFirstNonEmpty("ALLSTAK_COMMIT_SHA", "GIT_COMMIT", "VERCEL_GIT_COMMIT_SHA", "RAILWAY_GIT_COMMIT_SHA", "RENDER_GIT_COMMIT")
	}
	if c.Branch == "" {
		c.Branch = envFirstNonEmpty("ALLSTAK_BRANCH", "GIT_BRANCH", "VERCEL_GIT_COMMIT_REF", "RAILWAY_GIT_BRANCH")
	}
	return c
}

// envOfflineQueue lets deployments toggle the offline spool without a code
// change (e.g. ALLSTAK_OFFLINE_QUEUE=0 in a known-ephemeral container). An
// explicit Config flag always wins over the env var.
const envOfflineQueue = "ALLSTAK_OFFLINE_QUEUE"

// shouldEnableOfflineQueue resolves the EnableOfflineQueue flag. Precedence:
//  1. explicit Config flag (non-nil) — always wins
//  2. ALLSTAK_OFFLINE_QUEUE env var ("0"/"false"/"off" disables; anything
//     else truthy enables)
//  3. default: ON for normal binaries, OFF under the Go test runtime (binary
//     name ends in ".test") so the SDK's own and host applications' unit tests
//     don't write to the real user cache dir. Tests opt in explicitly with a
//     flag + a temp dir, mirroring the session-tracking test guard.
func shouldEnableOfflineQueue(flag *bool) bool {
	if flag != nil {
		return *flag
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envOfflineQueue))) {
	case "0", "false", "off", "no":
		return false
	case "1", "true", "on", "yes":
		return true
	}
	return !strings.HasSuffix(os.Args[0], ".test")
}

// envSendDefaultPii lets deployments toggle PII passthrough without a code
// change. An explicit Config flag always wins over the env var.
const envSendDefaultPii = "ALLSTAK_SEND_DEFAULT_PII"

// resolveSendDefaultPii resolves the SendDefaultPii flag. Precedence:
//  1. explicit Config flag (non-nil) — always wins
//  2. ALLSTAK_SEND_DEFAULT_PII env var ("1"/"true"/"on"/"yes" enables PII
//     passthrough; "0"/"false"/"off"/"no" forces the secure default)
//  3. default: FALSE — email/IPv4 value scrubbing is ON.
func resolveSendDefaultPii(flag *bool) bool {
	if flag != nil {
		return *flag
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envSendDefaultPii))) {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	}
	return false
}

// scrubOptions returns the resolved value-scrubbing policy for this config. The
// wire path always scrubs values; sendDefaultPii only gates the email/IPv4
// layer (credit-card + SSN are always on).
func (c Config) scrubOptions() scrubOptions {
	return scrubOptions{
		scrubValues:    true,
		sendDefaultPii: resolveSendDefaultPii(c.SendDefaultPii),
	}
}

// resolveHost returns the effective ingest base URL. Precedence:
//  1. ALLSTAK_HOST env var (for local dev / self-hosted / tests)
//  2. INGEST_HOST constant (production)
//
// Trailing slashes are trimmed so callers can always append "/ingest/v1/...".
func resolveHost() string {
	if v := strings.TrimSpace(os.Getenv(envHostOverride)); v != "" {
		return strings.TrimRight(v, "/")
	}
	return strings.TrimRight(INGEST_HOST, "/")
}

// defaultServiceName returns the basename of os.Args[0] or "go-service" as a
// last-resort fallback. It never panics.
func defaultServiceName() string {
	if len(os.Args) == 0 {
		return "go-service"
	}
	bin := os.Args[0]
	// Strip any directory prefix (both "/" and "\\" to be safe on Windows).
	if i := strings.LastIndexAny(bin, `/\`); i >= 0 {
		bin = bin[i+1:]
	}
	// Strip .exe suffix on Windows.
	bin = strings.TrimSuffix(bin, ".exe")
	if bin == "" {
		return "go-service"
	}
	return bin
}
