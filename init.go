package allstak

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Default-client environment variables read by InitFromEnv. Only APIKey is
// strictly required for ingest; everything else falls back to applyDefaults.
const (
	envAPIKey      = "ALLSTAK_API_KEY"
	envEnvironment = "ALLSTAK_ENVIRONMENT"
	envServiceName = "ALLSTAK_SERVICE_NAME"
	envDebug       = "ALLSTAK_DEBUG"
	envSampleRate  = "ALLSTAK_SAMPLE_RATE"
	envTracesRate  = "ALLSTAK_TRACES_SAMPLE_RATE"
	envDist        = "ALLSTAK_DIST"
)

// defaultClient is the process-wide client installed by InitFromEnv / SetDefault
// and read by the package-level helpers (allstak.Go, allstak.HTTPClient, etc.).
// It is an atomic.Pointer so install/read is race-free without locking the hot
// path.
var defaultClient atomic.Pointer[Client]

// initOnce guards InitFromEnv so repeated calls (e.g. from main and a package
// init) don't spawn multiple worker fleets. The first call wins; later calls
// return the already-installed client.
var initOnce sync.Once

// InitFromEnv constructs a Client purely from ALLSTAK_* environment variables,
// installs it as the package default (so allstak.Go / allstak.HTTPClient /
// allstak.CaptureException work with zero wiring), and returns it. It is the
// near-zero-config entry point:
//
//	func main() {
//	    client := allstak.InitFromEnv()
//	    defer client.Close(context.Background())
//	    // breadcrumbs, panic capture, http/db instrumentation all default-on
//	}
//
// Recognized variables (all optional except ALLSTAK_API_KEY, without which the
// client runs in safe no-op mode):
//
//	ALLSTAK_API_KEY            project ingest key
//	ALLSTAK_HOST               ingest host override (read by resolveHost)
//	ALLSTAK_ENVIRONMENT        environment tag (default "production")
//	ALLSTAK_SERVICE_NAME       service name (default: binary name)
//	ALLSTAK_RELEASE            release/version (also auto-detected)
//	ALLSTAK_DEBUG              "1"/"true" enables stderr debug logging
//	ALLSTAK_SAMPLE_RATE        error sample rate, 0.0–1.0
//	ALLSTAK_TRACES_SAMPLE_RATE span sample rate, 0.0–1.0
//	ALLSTAK_DIST               build distribution tag
//
// Release/commit/branch are additionally auto-detected by applyDefaults, and
// the offline queue / session tracking / PII flags honor their own env vars.
//
// InitFromEnv is idempotent: the first call installs the default; later calls
// return that same client without creating another.
func InitFromEnv() *Client {
	var c *Client
	initOnce.Do(func() {
		c = New(configFromEnv())
		defaultClient.Store(c)
	})
	if c == nil {
		// A prior InitFromEnv already ran — return the installed default.
		c = Default()
	}
	return c
}

// configFromEnv builds a Config from the ALLSTAK_* environment. Unset values
// are left zero so applyDefaults fills them.
func configFromEnv() Config {
	cfg := Config{
		APIKey:      strings.TrimSpace(os.Getenv(envAPIKey)),
		Environment: strings.TrimSpace(os.Getenv(envEnvironment)),
		ServiceName: strings.TrimSpace(os.Getenv(envServiceName)),
		Release:     strings.TrimSpace(os.Getenv("ALLSTAK_RELEASE")),
		Dist:        strings.TrimSpace(os.Getenv(envDist)),
		Debug:       envTruthy(os.Getenv(envDebug)),
	}
	if v := strings.TrimSpace(os.Getenv(envSampleRate)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.SampleRate = f
		}
	}
	if v := strings.TrimSpace(os.Getenv(envTracesRate)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.TracesSampleRate = f
		}
	}
	return cfg
}

// envTruthy parses a boolean-ish env value. Unset/empty is false.
func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// SetDefault installs c as the package-default client used by the package-level
// helpers (Go, GoCtx, HTTPClient, CaptureException, etc.). Applications that
// build their Client explicitly via New can opt into the package-level API by
// calling SetDefault(client) once at startup. Passing nil clears the default.
func SetDefault(c *Client) { defaultClient.Store(c) }

// Default returns the package-default client installed by InitFromEnv or
// SetDefault, or nil if none has been installed. The package-level helpers are
// no-ops when it is nil.
func Default() *Client { return defaultClient.Load() }

// HTTPClient returns an *http.Client whose Transport is already wired with
// AllStak outbound capture (and trace-context propagation) over the given inner
// transport. Pass nil to wrap http.DefaultTransport. Every request made through
// the returned client is auto-instrumented with no per-call code:
//
//	httpClient := client.HTTPClient(nil)
//	resp, err := httpClient.Get("https://api.example.com/widgets")
//
// The returned client copies http.DefaultClient's zero-value behavior (no
// global timeout) so it is a drop-in replacement; set Timeout on it if you want
// one. The trace context is read from each request's context, so calls made
// with a request-scoped context (req.WithContext(ctx)) correlate automatically.
func (c *Client) HTTPClient(inner http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: NewTransport(c, inner),
	}
}

// HTTPClient is the package-level form over the default client. It returns a
// fully-instrumented *http.Client. If no default client is installed it returns
// a plain client wrapping the given inner transport (still functional, just not
// reporting) so callers never get a nil client back.
func HTTPClient(inner http.RoundTripper) *http.Client {
	if c := Default(); c != nil {
		return c.HTTPClient(inner)
	}
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &http.Client{Transport: inner}
}

// WrapHTTPClient instruments an EXISTING *http.Client in place by wrapping its
// current Transport with AllStak outbound capture. It preserves the client's
// Timeout, CheckRedirect, and Jar, so you can adopt instrumentation on a
// pre-configured client without rebuilding it:
//
//	allstak.WrapHTTPClient(client, myConfiguredClient)
//
// Returns the same *http.Client for chaining. A nil hc returns a fresh
// instrumented client.
func (c *Client) WrapHTTPClient(hc *http.Client) *http.Client {
	if hc == nil {
		return c.HTTPClient(nil)
	}
	hc.Transport = NewTransport(c, hc.Transport)
	return hc
}

// ── Package-level capture convenience over the default client ───────────────
//
// These let zero-config callers (InitFromEnv users) report without threading a
// *Client through every layer. Each is a no-op when no default is installed.

// CaptureException reports err through the default client. No-op if no default
// client is installed.
func CaptureException(ctx context.Context, err error) {
	if c := Default(); c != nil {
		c.CaptureException(ctx, err)
	}
}

// CaptureMessage reports a plain-text event through the default client. No-op
// if no default client is installed.
func CaptureMessage(ctx context.Context, level, message string) {
	if c := Default(); c != nil {
		c.CaptureMessage(ctx, level, message)
	}
}

// AddBreadcrumb records a breadcrumb on the request-scoped trail through the
// default client. No-op if no default client is installed.
func AddBreadcrumb(ctx context.Context, bc Breadcrumb) {
	if c := Default(); c != nil {
		c.AddBreadcrumb(ctx, bc)
	}
}

// Close flushes and closes the default client, then clears it. No-op if no
// default client is installed. Useful in defer for InitFromEnv callers:
//
//	allstak.InitFromEnv()
//	defer allstak.Close(context.Background())
func Close(ctx context.Context) error {
	c := Default()
	if c == nil {
		return nil
	}
	defaultClient.Store(nil)
	return c.Close(ctx)
}
