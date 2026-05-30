package allstak

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConfigFromEnvReadsAllStakVars(t *testing.T) {
	t.Setenv(envAPIKey, "ask_env_key")
	t.Setenv(envEnvironment, "staging")
	t.Setenv(envServiceName, "env-svc")
	t.Setenv(envDebug, "true")
	t.Setenv(envSampleRate, "0.5")
	t.Setenv(envTracesRate, "0.25")
	t.Setenv(envDist, "linux-arm64")

	cfg := configFromEnv()
	if cfg.APIKey != "ask_env_key" {
		t.Fatalf("APIKey = %q", cfg.APIKey)
	}
	if cfg.Environment != "staging" {
		t.Fatalf("Environment = %q", cfg.Environment)
	}
	if cfg.ServiceName != "env-svc" {
		t.Fatalf("ServiceName = %q", cfg.ServiceName)
	}
	if !cfg.Debug {
		t.Fatal("Debug should be true")
	}
	if cfg.SampleRate != 0.5 {
		t.Fatalf("SampleRate = %v", cfg.SampleRate)
	}
	if cfg.TracesSampleRate != 0.25 {
		t.Fatalf("TracesSampleRate = %v", cfg.TracesSampleRate)
	}
	if cfg.Dist != "linux-arm64" {
		t.Fatalf("Dist = %q", cfg.Dist)
	}
}

func TestSetDefaultAndPackageHelpers(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{APIKey: "ask_test"}, rt)

	SetDefault(c)
	defer SetDefault(nil)

	if Default() != c {
		t.Fatal("Default() did not return the installed client")
	}

	// Package-level CaptureException routes through the default client.
	CaptureException(context.Background(), errTest("pkg boom"))
	sends := rt.waitFor(t, 1)
	p := sends[0].payload.(*ErrorPayload)
	if p.Message != "pkg boom" {
		t.Fatalf("message = %q", p.Message)
	}
}

func TestPackageHelpersNoopWithoutDefault(t *testing.T) {
	SetDefault(nil)
	// All of these must be safe no-ops when no default is installed.
	CaptureException(context.Background(), errTest("ignored"))
	CaptureMessage(context.Background(), "info", "ignored")
	AddBreadcrumb(context.Background(), Breadcrumb{Message: "ignored"})
	if err := Close(context.Background()); err != nil {
		t.Fatalf("Close with no default should be nil, got %v", err)
	}
	Go(func() {})
}

func TestHTTPClientWrapsOutboundCapture(t *testing.T) {
	rt := &recordingTransport{}
	// BatchSize 1 so the outbound HTTP batch flushes immediately on capture
	// (the batched streams only emit on a full batch or the flush tick).
	c := newTestClient(t, Config{APIKey: "ask_test", BatchSize: 1}, rt)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	hc := c.HTTPClient(nil)
	resp, err := hc.Get(upstream.URL + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Expect an outbound HTTP request to have been captured (BatchSize 1 flushes
	// it on capture; poll briefly to avoid racing the worker goroutine).
	deadline := time.Now().Add(time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		rt.mu.Lock()
		for _, s := range rt.sends {
			if b, ok := s.payload.(HTTPRequestBatch); ok {
				for _, item := range b.Requests {
					if item.Direction == "outbound" {
						found = true
					}
				}
			}
		}
		rt.mu.Unlock()
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("expected an outbound HTTP capture")
	}
}

func TestWrapHTTPClientPreservesTimeout(t *testing.T) {
	rt := &recordingTransport{}
	c := newTestClient(t, Config{APIKey: "ask_test"}, rt)

	orig := &http.Client{Timeout: 7 * time.Second}
	wrapped := c.WrapHTTPClient(orig)
	if wrapped != orig {
		t.Fatal("WrapHTTPClient should return the same client")
	}
	if wrapped.Timeout != 7*time.Second {
		t.Fatalf("timeout not preserved: %v", wrapped.Timeout)
	}
	if _, ok := wrapped.Transport.(*outboundTransport); !ok {
		t.Fatalf("transport not wrapped: %T", wrapped.Transport)
	}
}
