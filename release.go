package allstak

import (
	"context"
	"os/exec"
	"runtime/debug"
	"strings"
	"time"
)

// buildVCSInfo is the subset of runtime/debug.BuildInfo the release detector
// needs. Extracting it behind an interface keeps detectReleaseFromBuildInfo a
// pure, table-testable function: tests inject a fake instead of needing a real
// VCS-stamped binary.
type buildVCSInfo struct {
	// revision is the value of the "vcs.revision" build setting (full SHA), or
	// "" when the toolchain stamped no VCS info (e.g. `go run` outside a repo,
	// or `-buildvcs=false`).
	revision string
	// modified is true when "vcs.modified" == "true" (working tree was dirty
	// at build time).
	modified bool
	// time is the value of the "vcs.time" build setting (RFC3339), or "".
	time string
}

// readBuildVCSInfo reads the VCS build settings the Go toolchain automatically
// stamps into the binary at `go build` time. It returns ok=false when no build
// info or no VCS revision is available. This is the real implementation; the
// detector takes it as a seam so tests can substitute a fake.
func readBuildVCSInfo() (buildVCSInfo, bool) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return buildVCSInfo{}, false
	}
	var info buildVCSInfo
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.revision = s.Value
		case "vcs.modified":
			info.modified = s.Value == "true"
		case "vcs.time":
			info.time = s.Value
		}
	}
	if info.revision == "" {
		return buildVCSInfo{}, false
	}
	return info, true
}

// detectReleaseFromBuildInfo builds a release string from VCS build info.
// Pure and seamable: it does no I/O. Returns "" when info carries no revision.
//
// Format: the revision shortened to 12 hex chars, with "-dirty" appended when
// the build tree was modified. e.g. "a1b2c3d4e5f6" or "a1b2c3d4e5f6-dirty".
func detectReleaseFromBuildInfo(info buildVCSInfo, ok bool) string {
	if !ok || info.revision == "" {
		return ""
	}
	rev := info.revision
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if info.modified {
		rev += "-dirty"
	}
	return rev
}

// gitRunner runs a git command and returns its trimmed stdout. The real
// implementation shells out; tests inject a fake that returns canned output or
// an error. Returning an error (or empty output) makes the detector fall
// through gracefully.
type gitRunner func(args ...string) (string, error)

// defaultGitRunner shells out to `git` with a short timeout. Any failure
// (git not installed, not a repo, timeout) surfaces as an error so the caller
// falls through to the version fallback. It never panics.
func defaultGitRunner(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// detectReleaseFromGit derives a release via `git describe`. Pure w.r.t. the
// injected runner: tests pass a fake runner. Returns "" on any error or empty
// output so the caller can fall through to the version fallback.
func detectReleaseFromGit(run gitRunner) string {
	if run == nil {
		return ""
	}
	out, err := run("describe", "--tags", "--always", "--dirty")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// resolveAutoRelease implements step 3 of release resolution: automatic,
// CI-free detection. It is a pure unit parameterized by its two seams so tests
// never touch a real repo.
//
// PRIMARY: VCS info the Go toolchain stamps at build time (no CI, no runtime
// git). SECONDARY: a guarded `git describe` shell-out, used only when the
// binary carries no VCS info (e.g. `go run` without a repo). Returns "" when
// neither yields anything; the caller then applies the version fallback.
func resolveAutoRelease(readInfo func() (buildVCSInfo, bool), run gitRunner) string {
	if readInfo != nil {
		if rel := detectReleaseFromBuildInfo(readInfo()); rel != "" {
			return rel
		}
	}
	return detectReleaseFromGit(run)
}
