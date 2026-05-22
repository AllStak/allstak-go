package allstak

import (
	"testing"
	"time"
)

func TestConfigApplyDefaults(t *testing.T) {
	cfg := Config{}.applyDefaults()

	if cfg.Environment != "production" {
		t.Fatalf("Environment = %q, want production", cfg.Environment)
	}
	if cfg.Platform != "go" {
		t.Fatalf("Platform = %q, want go", cfg.Platform)
	}
	if cfg.SDKName != SDKName {
		t.Fatalf("SDKName = %q, want %q", cfg.SDKName, SDKName)
	}
	if cfg.SDKVersion != SDKVersion {
		t.Fatalf("SDKVersion = %q, want %q", cfg.SDKVersion, SDKVersion)
	}
	if cfg.FlushInterval != 2*time.Second {
		t.Fatalf("FlushInterval = %s, want 2s", cfg.FlushInterval)
	}
	if cfg.BatchSize != 50 {
		t.Fatalf("BatchSize = %d, want 50", cfg.BatchSize)
	}
	if cfg.QueueCapacity != 1000 {
		t.Fatalf("QueueCapacity = %d, want 1000", cfg.QueueCapacity)
	}
	if cfg.MaxRetries != 3 {
		t.Fatalf("MaxRetries = %d, want 3", cfg.MaxRetries)
	}
}

func TestReleaseTags(t *testing.T) {
	cfg := Config{
		SDKName:    "allstak-go",
		SDKVersion: "1.2.3",
		Platform:   "go",
		Dist:       "linux-amd64",
		CommitSha:  "abc123",
		Branch:     "main",
	}

	tags := cfg.ReleaseTags()
	want := map[string]string{
		"sdk.name":      "allstak-go",
		"sdk.version":   "1.2.3",
		"platform":      "go",
		"dist":          "linux-amd64",
		"commit.sha":    "abc123",
		"commit.branch": "main",
	}

	for key, value := range want {
		if tags[key] != value {
			t.Fatalf("tags[%q] = %q, want %q", key, tags[key], value)
		}
	}
}
