package allstak

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEnsureSecureDirCreates0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := ensureSecureDir(dir); err != nil {
		t.Fatalf("ensureSecureDir: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %o, want 0700", fi.Mode().Perm())
	}
}

func TestEnsureSecureDirTightensPreexistingLooseDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil { // bypass umask
		t.Fatalf("chmod: %v", err)
	}
	if err := ensureSecureDir(dir); err != nil {
		t.Fatalf("ensureSecureDir: %v", err)
	}
	fi, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("pre-existing 0777 dir not tightened: mode = %o", fi.Mode().Perm())
	}
}

func TestEnsureSecureDirRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := ensureSecureDir(link); err == nil {
		t.Fatal("ensureSecureDir must refuse a symlinked dir")
	}
}

func TestEnsureSecureDirRefusesRegularFile(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := ensureSecureDir(file); err == nil {
		t.Fatal("ensureSecureDir must refuse a regular file")
	}
}

func TestNewSpoolRefusesSymlinkedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "spool-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if s := newSpool(link, 10, 1<<20, time.Hour, nil); s != nil {
		t.Fatal("newSpool must refuse a symlinked dir (degrade to in-memory)")
	}
	// A legitimate dir still works.
	if s := newSpool(filepath.Join(base, "ok"), 10, 1<<20, time.Hour, nil); s == nil {
		t.Fatal("newSpool refused a legitimate dir")
	}
}

func TestResolveSpoolDirTempFallbackIsPerUID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uid semantics")
	}
	// Force os.UserCacheDir to fail so the world-tmp fallback engages.
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	dir := resolveSpoolDir("")
	want := spoolDirName + "-" + strconv.Itoa(os.Getuid())
	if filepath.Base(dir) != want {
		t.Fatalf("temp-fallback spool dir = %q, want basename %q (per-uid)", dir, want)
	}
}

func TestSessionStatePathDefaultsArePerUser(t *testing.T) {
	// sessionStatePath returns "" under the test binary unless OfflineQueueDir
	// is set; temporarily disguise the binary name to exercise the default.
	origArg0 := os.Args[0]
	os.Args[0] = "real-service"
	defer func() { os.Args[0] = origArg0 }()

	// With a user cache dir available, state lives under it.
	p := sessionStatePath(Config{APIKey: "k", Release: "r", Environment: "e"})
	if p == "" {
		t.Fatal("sessionStatePath empty for a non-test binary")
	}
	if cache, err := os.UserCacheDir(); err == nil && cache != "" {
		if !strings.HasPrefix(p, filepath.Join(cache, sessionStateDirectory)) {
			t.Fatalf("session state %q not under user cache dir %q", p, cache)
		}
	}

	// With no resolvable cache dir, the tmp fallback embeds the uid.
	if runtime.GOOS != "windows" {
		t.Setenv("HOME", "")
		t.Setenv("XDG_CACHE_HOME", "")
		p = sessionStatePath(Config{APIKey: "k", Release: "r", Environment: "e"})
		wantDir := sessionStateDirectory + "-" + strconv.Itoa(os.Getuid())
		if filepath.Base(filepath.Dir(p)) != wantDir {
			t.Fatalf("tmp-fallback session dir = %q, want %q (per-uid)", filepath.Dir(p), wantDir)
		}
	}
}

func TestReadSessionStateRefusesSymlinkedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"sessionId":"abc","startedAt":1,"updatedAt":1,"status":"ok"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "state.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	c := &Client{session: &sessionTracker{statePath: link}}
	if _, ok := c.readSessionState(); ok {
		t.Fatal("readSessionState must refuse a symlinked state file")
	}

	// The same state read through a real file is accepted (sanity).
	c2 := &Client{session: &sessionTracker{statePath: target}}
	if _, ok := c2.readSessionState(); !ok {
		t.Fatal("readSessionState rejected a legitimate state file")
	}
}

func TestWriteSessionStateUsesRandomTempName(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	// Pre-plant a file at the old predictable temp name; the writer must not
	// touch it (it now uses CreateTemp with a random name).
	planted := statePath + ".tmp"
	if err := os.WriteFile(planted, []byte("planted"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := &Client{session: &sessionTracker{statePath: statePath}}
	c.writeSessionState(persistedSessionState{
		Version:         sessionStateVersion,
		SessionID:       "abc",
		StartedAtUnixMs: 1,
		UpdatedAtUnixMs: 1,
		Status:          statusOK,
	})

	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state not written: %v", err)
	}
	got, err := os.ReadFile(planted)
	if err != nil || string(got) != "planted" {
		t.Fatalf("predictable .tmp name was written through: %q err=%v", got, err)
	}
}
