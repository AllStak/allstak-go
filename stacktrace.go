package allstak

import (
	"fmt"
	"runtime"
	"strings"
)

// maxStackDepth is the maximum number of frames captured per stack trace.
// Deep recursion beyond this is silently truncated — the dashboard rarely
// needs more context than this to group an error.
const maxStackDepth = 64

// captureStack returns a stack trace as a slice of human-readable frame
// strings in the format "<file>:<line> <function>". Frames inside this
// SDK itself are filtered out so the trace starts at user code.
//
// The `skip` parameter is how many additional frames above the caller to
// discard — useful when capture functions wrap each other.
func captureStack(skip int) []string {
	// Grab more than we need so we can filter SDK frames without losing
	// user frames to the tail of the slice.
	pcs := make([]uintptr, maxStackDepth+8)
	// 2 = skip runtime.Callers itself and captureStack.
	n := runtime.Callers(2+skip, pcs)
	if n == 0 {
		return nil
	}
	frames := runtime.CallersFrames(pcs[:n])

	out := make([]string, 0, n)
	for {
		frame, more := frames.Next()
		if !isSDKFrame(frame.Function) {
			out = append(out, formatFrame(frame))
		}
		if !more || len(out) >= maxStackDepth {
			break
		}
	}
	return out
}

// isSDKFrame reports whether a function name belongs to this SDK package
// and should be skipped from user-facing stack traces. This keeps the
// "Capture" wrapper and middleware helpers out of the trace so grouping
// is stable across call sites.
func isSDKFrame(fn string) bool {
	if fn == "" {
		return false
	}
	return strings.HasPrefix(fn, "github.com/AllStak/allstak-go")
}

// formatFrame turns a runtime.Frame into "<file>:<line> <function>".
// The file is left as the absolute path — the dashboard normalizes it.
func formatFrame(f runtime.Frame) string {
	return fmt.Sprintf("%s:%d %s", f.File, f.Line, f.Function)
}

// captureStructuredFrames returns Phase 2 v2 ingest frames alongside the
// v1 string list. Same skip semantics as captureStack.
func captureStructuredFrames(skip int) []Frame {
	pcs := make([]uintptr, maxStackDepth+8)
	n := runtime.Callers(2+skip, pcs)
	if n == 0 {
		return nil
	}
	frames := runtime.CallersFrames(pcs[:n])
	out := make([]Frame, 0, n)
	for {
		f, more := frames.Next()
		if !isSDKFrame(f.Function) {
			out = append(out, Frame{
				Filename: f.File,
				AbsPath:  f.File,
				Function: f.Function,
				Lineno:   f.Line,
				InApp:    isInAppFrame(f.Function, f.File),
				Platform: "go",
			})
		}
		if !more || len(out) >= maxStackDepth {
			break
		}
	}
	return out
}

// isInAppFrame heuristic — Go's stdlib + vendored deps live under
// /usr/local/go/ or /go/pkg/mod/, customer code does not.
func isInAppFrame(function, file string) bool {
	if file == "" {
		return true
	}
	if strings.Contains(file, "/go/pkg/mod/") || strings.Contains(file, "/go/src/") {
		return false
	}
	if strings.HasPrefix(function, "runtime.") || strings.HasPrefix(function, "reflect.") {
		return false
	}
	return true
}

// exceptionClassOf tries to produce a stable "class name" for an error,
// mirroring the way other SDKs report `ExceptionClass` to the backend.
// For a plain errors.New, this returns "*errors.errorString"; for a user
// type it returns the package-qualified type name.
func exceptionClassOf(err error) string {
	if err == nil {
		return "error"
	}
	// fmt.Sprintf("%T", ...) is the canonical Go way to get a type name.
	return fmt.Sprintf("%T", err)
}
