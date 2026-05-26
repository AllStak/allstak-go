package allstak

import (
	"errors"
	"testing"
)

func TestDetectReleaseFromBuildInfo(t *testing.T) {
	tests := []struct {
		name string
		info buildVCSInfo
		ok   bool
		want string
	}{
		{
			name: "short revision used as-is",
			info: buildVCSInfo{revision: "abc123"},
			ok:   true,
			want: "abc123",
		},
		{
			name: "long revision shortened to 12 chars",
			info: buildVCSInfo{revision: "a1b2c3d4e5f60718293a4b5c6d7e8f90"},
			ok:   true,
			want: "a1b2c3d4e5f6",
		},
		{
			name: "modified appends -dirty",
			info: buildVCSInfo{revision: "a1b2c3d4e5f60718293a4b5c6d7e8f90", modified: true},
			ok:   true,
			want: "a1b2c3d4e5f6-dirty",
		},
		{
			name: "no vcs info returns empty",
			info: buildVCSInfo{},
			ok:   false,
			want: "",
		},
		{
			name: "ok but empty revision returns empty",
			info: buildVCSInfo{revision: ""},
			ok:   true,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectReleaseFromBuildInfo(tt.info, tt.ok); got != tt.want {
				t.Fatalf("detectReleaseFromBuildInfo = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectReleaseFromGit(t *testing.T) {
	t.Run("runner returns describe output", func(t *testing.T) {
		run := func(args ...string) (string, error) { return "v1.2.3-4-gabc123\n", nil }
		if got := detectReleaseFromGit(run); got != "v1.2.3-4-gabc123" {
			t.Fatalf("got %q, want trimmed describe output", got)
		}
	})
	t.Run("runner error falls through to empty", func(t *testing.T) {
		run := func(args ...string) (string, error) { return "", errors.New("not a git repo") }
		if got := detectReleaseFromGit(run); got != "" {
			t.Fatalf("got %q, want empty on error", got)
		}
	})
	t.Run("nil runner returns empty", func(t *testing.T) {
		if got := detectReleaseFromGit(nil); got != "" {
			t.Fatalf("got %q, want empty for nil runner", got)
		}
	})
}

func TestResolveAutoRelease(t *testing.T) {
	t.Run("build info wins over git", func(t *testing.T) {
		readInfo := func() (buildVCSInfo, bool) { return buildVCSInfo{revision: "deadbeefcafe1234", modified: true}, true }
		run := func(args ...string) (string, error) { return "v9.9.9", nil }
		if got := resolveAutoRelease(readInfo, run); got != "deadbeefcafe-dirty" {
			t.Fatalf("got %q, want build-info-derived release", got)
		}
	})
	t.Run("falls back to git when no build info", func(t *testing.T) {
		readInfo := func() (buildVCSInfo, bool) { return buildVCSInfo{}, false }
		run := func(args ...string) (string, error) { return "v1.0.0", nil }
		if got := resolveAutoRelease(readInfo, run); got != "v1.0.0" {
			t.Fatalf("got %q, want git fallback", got)
		}
	})
	t.Run("empty when neither yields", func(t *testing.T) {
		readInfo := func() (buildVCSInfo, bool) { return buildVCSInfo{}, false }
		run := func(args ...string) (string, error) { return "", errors.New("nope") }
		if got := resolveAutoRelease(readInfo, run); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

// TestReleaseResolutionPrecedence exercises the full ordering through
// applyDefaults: explicit > env > detected > version, plus the opt-out.
func TestReleaseResolutionPrecedence(t *testing.T) {
	t.Run("explicit release always wins", func(t *testing.T) {
		t.Setenv("ALLSTAK_RELEASE", "from-env")
		cfg := Config{Release: "explicit-v1"}.applyDefaults()
		if cfg.Release != "explicit-v1" {
			t.Fatalf("Release = %q, want explicit-v1", cfg.Release)
		}
	})
	t.Run("env wins over auto-detect", func(t *testing.T) {
		t.Setenv("ALLSTAK_RELEASE", "from-env")
		cfg := Config{}.applyDefaults()
		if cfg.Release != "from-env" {
			t.Fatalf("Release = %q, want from-env", cfg.Release)
		}
	})
	t.Run("auto-detect or version fallback when no explicit/env", func(t *testing.T) {
		t.Setenv("ALLSTAK_RELEASE", "")
		t.Setenv("VERCEL_GIT_COMMIT_SHA", "")
		t.Setenv("RAILWAY_GIT_COMMIT_SHA", "")
		t.Setenv("RENDER_GIT_COMMIT", "")
		cfg := Config{}.applyDefaults()
		// In `go test` the binary carries no VCS stamp and CWD is a repo, so
		// this resolves to a git-describe value or the SDKVersion fallback —
		// either way it must never be empty when detection is on.
		if cfg.Release == "" {
			t.Fatalf("Release is empty; auto-detect+fallback should guarantee non-empty")
		}
	})
	t.Run("opt-out disables detection and fallback", func(t *testing.T) {
		t.Setenv("ALLSTAK_RELEASE", "")
		t.Setenv("VERCEL_GIT_COMMIT_SHA", "")
		t.Setenv("RAILWAY_GIT_COMMIT_SHA", "")
		t.Setenv("RENDER_GIT_COMMIT", "")
		off := false
		cfg := Config{AutoDetectRelease: &off}.applyDefaults()
		if cfg.Release != "" {
			t.Fatalf("Release = %q, want empty when auto-detect off", cfg.Release)
		}
	})
}
