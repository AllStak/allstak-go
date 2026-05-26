package allstak

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Ingest endpoint paths — relative to the resolved host.
const (
	pathErrors       = "/ingest/v1/errors"
	pathLogs         = "/ingest/v1/logs"
	pathHTTPRequests = "/ingest/v1/http-requests"
	pathDBQueries    = "/ingest/v1/db"
	pathSpans        = "/ingest/v1/spans"
	pathHeartbeat    = "/ingest/v1/heartbeat"
)

// httpTransport is the low-level HTTP ingest transport. It knows how to
// serialize a payload, attach auth, and retry on transient failures. It
// does NOT batch — batching happens in the queue/worker layer above.
//
// The transport is safe for concurrent use. http.Client is already
// goroutine-safe, and this struct holds no mutable state post-construction.
type httpTransport struct {
	host       string
	apiKey     string
	httpClient *http.Client
	maxRetries int
	debug      bool
}

// newHTTPTransport constructs a transport wired to the given host. A nil
// httpClient uses a fresh default client with the configured timeout.
func newHTTPTransport(host, apiKey string, timeout time.Duration, maxRetries int, debug bool) *httpTransport {
	return &httpTransport{
		host:       host,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: timeout},
		maxRetries: maxRetries,
		debug:      debug,
	}
}

// send marshals payload to JSON and POSTs it to host+path with the
// X-AllStak-Key header. Transient failures (network errors, 5xx, 429) are
// retried with exponential backoff + jitter up to maxRetries times. A 4xx
// other than 429 is treated as permanent and returned immediately. Success
// is any 2xx. The context can cancel any outstanding retry.
func (t *httpTransport) send(ctx context.Context, path string, payload any) error {
	if t.apiKey == "" {
		// No-op mode — silently drop so the SDK is safe to include without
		// configuration in tests and local scripts.
		return nil
	}

	// Scrub PII / secrets from user-supplied maps exactly once, here at the
	// single wire chokepoint, before marshalling. Fail-open: if scrubbing
	// panics for any reason we fall back to the original payload rather than
	// dropping the event.
	payload = scrubPayloadSafe(payload)

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("allstak: marshal payload: %w", err)
	}

	url := t.host + path
	var lastErr error
	// retryAfter carries a server-directed delay (from a 429/503 Retry-After
	// header) into the next attempt's wait. Zero means "no server hint — use
	// exponential backoff".
	var retryAfter time.Duration

	for attempt := 0; attempt <= t.maxRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if attempt > 0 {
			var sleep time.Duration
			if retryAfter > 0 {
				// Honor the server's Retry-After hint, clamped to maxRetryAfter.
				sleep = retryAfter
				retryAfter = 0
			} else {
				// Exponential backoff: 100ms, 200ms, 400ms, ... capped at 5s
				// with ±25% jitter to avoid thundering herds.
				base := time.Duration(100*(1<<uint(attempt-1))) * time.Millisecond
				if base > 5*time.Second {
					base = 5 * time.Second
				}
				jitter := time.Duration(rand.Int63n(int64(base / 2)))
				sleep = base - base/4 + jitter
			}
			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("allstak: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-AllStak-Key", t.apiKey)
		req.Header.Set("User-Agent", "allstak-go/"+sdkVersion)

		resp, err := t.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("allstak: http post %s: %w", path, err)
			if t.debug {
				fmt.Fprintf(stderrWriter, "[allstak] transport attempt %d failed: %v\n", attempt+1, err)
			}
			continue
		}

		// Drain and close the body regardless of outcome so the connection
		// can be reused by keep-alive.
		respBody, _ := io.ReadAll(resp.Body)
		retryAfterHeader := resp.Header.Get("Retry-After")
		_ = resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}

		// 4xx (except 429) is permanent — no point retrying an invalid key
		// or a malformed payload.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return fmt.Errorf("allstak: ingest %s returned %d: %s", path, resp.StatusCode, truncate(string(respBody), 300))
		}

		// On 429 (rate limited) and 503 (unavailable) honor a Retry-After
		// header if the server sent a parseable one; otherwise the next
		// attempt falls back to exponential backoff.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			retryAfter = parseRetryAfter(retryAfterHeader, time.Now())
		}

		lastErr = fmt.Errorf("allstak: ingest %s returned %d", path, resp.StatusCode)
		if t.debug {
			fmt.Fprintf(stderrWriter, "[allstak] transport attempt %d got %d: %s\n", attempt+1, resp.StatusCode, truncate(string(respBody), 200))
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("allstak: ingest %s exhausted %d retries", path, t.maxRetries)
	}
	return lastErr
}

// maxRetryAfter clamps any server-directed Retry-After delay. A hostile or
// misconfigured server could otherwise pin a worker for a very long time.
const maxRetryAfter = 300 * time.Second

// parseRetryAfter interprets an HTTP Retry-After header value relative to now
// per RFC 7231 §7.1.3. It accepts either a non-negative integer number of
// seconds ("120") or an HTTP-date ("Wed, 21 Oct 2015 07:28:00 GMT"), and
// returns the resulting delay.
//
// It returns 0 when the header is empty, unparseable, or resolves to a
// non-positive delay — the caller treats 0 as "no hint, use backoff". The
// returned delay is clamped to maxRetryAfter. This function is pure (now is
// injected) so it is fully unit-testable without sleeping.
func parseRetryAfter(header string, now time.Time) time.Duration {
	h := strings.TrimSpace(header)
	if h == "" {
		return 0
	}

	// Integer seconds form.
	if secs, err := strconv.Atoi(h); err == nil {
		if secs <= 0 {
			return 0
		}
		d := time.Duration(secs) * time.Second
		if d > maxRetryAfter {
			return maxRetryAfter
		}
		return d
	}

	// HTTP-date form.
	if when, err := http.ParseTime(h); err == nil {
		d := when.Sub(now)
		if d <= 0 {
			return 0
		}
		if d > maxRetryAfter {
			return maxRetryAfter
		}
		return d
	}

	// Garbage — fall back to backoff.
	return 0
}

// truncate returns s clipped to n runes with a "..." suffix if clipped.
// Used purely for bounded error messages; callers should never rely on it
// for safety-critical truncation.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// stderrWriter is indirected so tests can capture debug output if needed.
// It's a package-level var instead of a constructor argument because debug
// output is inherently a side channel.
var stderrWriter io.Writer = io.Discard
