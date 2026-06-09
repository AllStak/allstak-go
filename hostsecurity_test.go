package allstak

import (
	"context"
	"strings"
	"testing"
)

func TestValidateIngestHost(t *testing.T) {
	cases := []struct {
		name          string
		host          string
		allowInsecure bool
		wantErr       bool
	}{
		{"https production", "https://api.allstak.sa", false, false},
		{"https any host", "https://ingest.internal.example.com:8443", false, false},
		{"http localhost", "http://localhost:8080", false, false},
		{"http 127.0.0.1", "http://127.0.0.1:8080", false, false},
		{"http 127.0.0.53 loopback range", "http://127.0.0.53", false, false},
		{"http ::1", "http://[::1]:8080", false, false},
		{"http 0.0.0.0", "http://0.0.0.0:8080", false, false},
		{"http *.localhost", "http://api.localhost:3000", false, false},
		{"http non-loopback rejected", "http://ingest.internal.example.com", false, true},
		{"http public ip rejected", "http://10.1.2.3:8080", false, true},
		{"http non-loopback allowed via escape hatch", "http://ingest.internal.example.com", true, false},
		{"ftp scheme rejected", "ftp://api.allstak.sa", false, true},
		{"schemeless rejected", "api.allstak.sa", false, true},
		{"empty scheme with port rejected", "//api.allstak.sa", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			insecureHostWarned.Store(true) // silence the loud warning in tests
			err := validateIngestHost(tc.host, tc.allowInsecure)
			if tc.wantErr && err == nil {
				t.Fatalf("validateIngestHost(%q, %v) = nil, want error", tc.host, tc.allowInsecure)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateIngestHost(%q, %v) = %v, want nil", tc.host, tc.allowInsecure, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "rejected") {
				t.Fatalf("error %q should mention rejection", err.Error())
			}
		})
	}
}

func TestInitRejectsInsecureNonLoopbackHost(t *testing.T) {
	t.Setenv(envHostOverride, "http://ingest.internal.example.com")
	t.Setenv(envAllowInsecureHost, "")
	client, err := Init(Config{APIKey: "ask_test_key"})
	if err == nil {
		if client != nil {
			_ = client.Close(context.Background())
		}
		t.Fatal("Init with insecure host should fail")
	}
	if client != nil {
		t.Fatal("Init must not return a client alongside an error")
	}
	if !strings.Contains(err.Error(), "insecure ingest host 'http://ingest.internal.example.com' rejected") {
		t.Fatalf("error not descriptive: %v", err)
	}
	if !strings.Contains(err.Error(), "AllowInsecureHost") {
		t.Fatalf("error should mention the escape hatch: %v", err)
	}
}

func TestInitAcceptsHTTPSAndLoopbackHTTP(t *testing.T) {
	for _, host := range []string{"https://api.allstak.sa", "http://localhost:8080", "http://127.0.0.1:9999"} {
		t.Setenv(envHostOverride, host)
		client, err := Init(Config{})
		if err != nil {
			t.Fatalf("Init with host %q: %v", host, err)
		}
		_ = client.Close(context.Background())
	}
}

func TestInitAllowInsecureHostConfigFlag(t *testing.T) {
	t.Setenv(envHostOverride, "http://ingest.internal.example.com")
	insecureHostWarned.Store(false)
	client, err := Init(Config{AllowInsecureHost: true})
	if err != nil {
		t.Fatalf("Init with AllowInsecureHost: %v", err)
	}
	defer client.Close(context.Background())
	if !insecureHostWarned.Load() {
		t.Fatal("allowing an insecure host must emit the one-time warning")
	}
}

func TestInitAllowInsecureHostEnvVar(t *testing.T) {
	t.Setenv(envHostOverride, "http://ingest.internal.example.com")
	t.Setenv(envAllowInsecureHost, "1")
	insecureHostWarned.Store(true) // silence the warning
	client, err := Init(Config{})
	if err != nil {
		t.Fatalf("Init with ALLSTAK_ALLOW_INSECURE_HOST=1: %v", err)
	}
	_ = client.Close(context.Background())
}

func TestNewDegradesToDisabledNoopOnInsecureHost(t *testing.T) {
	t.Setenv(envHostOverride, "http://ingest.internal.example.com")
	t.Setenv(envAllowInsecureHost, "")
	client := New(Config{APIKey: "ask_test_key"})
	if client == nil {
		t.Fatal("New must never return nil")
	}
	defer client.Close(context.Background())

	// Captures must be safe no-ops: the API key is cleared and the transport
	// is inert, so nothing can reach the network and nothing can panic.
	if got := client.Config().APIKey; got != "" {
		t.Fatalf("disabled client kept APIKey %q, want empty (no-op mode)", got)
	}
	client.CaptureError(ErrorPayload{ExceptionClass: "x", Message: "y"})
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("Flush on disabled client: %v", err)
	}
}

func TestRunningInContainerKubernetesEnv(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	if !runningInContainer() {
		t.Fatal("KUBERNETES_SERVICE_HOST should mark the process as containerized")
	}
}

func TestApplyDefaultsDisableGitDetectionStillResolvesRelease(t *testing.T) {
	t.Setenv("ALLSTAK_RELEASE", "")
	t.Setenv("VERCEL_GIT_COMMIT_SHA", "")
	t.Setenv("RAILWAY_GIT_COMMIT_SHA", "")
	t.Setenv("RENDER_GIT_COMMIT", "")
	cfg := Config{DisableGitDetection: true}.applyDefaults()
	if cfg.Release == "" {
		t.Fatal("Release must never resolve empty (build-info or SDKVersion fallback)")
	}
}
