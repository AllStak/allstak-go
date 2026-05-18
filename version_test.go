package allstak

import "testing"

// TestVersionConsistency asserts that the exported SDKVersion (used in wire
// payloads) matches the internal sdkVersion constant (used in User-Agent).
// If someone adds a second version literal this test will catch the drift.
func TestVersionConsistency(t *testing.T) {
	if SDKVersion != sdkVersion {
		t.Fatalf("version mismatch: SDKVersion=%q (config.go) vs sdkVersion=%q (client.go)", SDKVersion, sdkVersion)
	}
}

// TestVersionNonEmpty guards against accidentally blanking the version.
func TestVersionNonEmpty(t *testing.T) {
	if sdkVersion == "" {
		t.Fatal("sdkVersion must not be empty")
	}
}

// TestDefaultConfigVersion ensures applyDefaults stamps the correct version.
func TestDefaultConfigVersion(t *testing.T) {
	cfg := Config{APIKey: "ask_test"}.applyDefaults()
	if cfg.SDKVersion != sdkVersion {
		t.Fatalf("applyDefaults set SDKVersion=%q, want %q", cfg.SDKVersion, sdkVersion)
	}
}
