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
}

// SDK identity sent on the wire as `sdk.name` / `sdk.version`.
const (
	SDKName    = "allstak-go"
	SDKVersion = "0.2.0"
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
	if c.Release == "" {
		c.Release = envFirstNonEmpty("ALLSTAK_RELEASE", "VERCEL_GIT_COMMIT_SHA", "RAILWAY_GIT_COMMIT_SHA", "RENDER_GIT_COMMIT")
	}
	if c.CommitSha == "" {
		c.CommitSha = envFirstNonEmpty("ALLSTAK_COMMIT_SHA", "GIT_COMMIT", "VERCEL_GIT_COMMIT_SHA", "RAILWAY_GIT_COMMIT_SHA", "RENDER_GIT_COMMIT")
	}
	if c.Branch == "" {
		c.Branch = envFirstNonEmpty("ALLSTAK_BRANCH", "GIT_BRANCH", "VERCEL_GIT_COMMIT_REF", "RAILWAY_GIT_BRANCH")
	}
	return c
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
