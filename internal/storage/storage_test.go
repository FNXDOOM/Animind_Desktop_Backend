package storage

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"animind-backend-go/internal/config"
)

func localService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db")
	t.Setenv("DESKTOP_TOKEN_SIGNING_KEY", "0123456789012345678901234567890123456789")
	t.Setenv("STORAGE_MODE", "local")
	t.Setenv("LOCAL_STORAGE_PATH", root)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	service, err := New(t.Context(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service, root
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestLocalPathRejectsEscape covers the ways a stored path could try to leave the library
// root. Every local read funnels through LocalPath, so this is the containment boundary for
// streaming, subtitles, audio variants and HLS sources.
func TestLocalPathRejectsEscape(t *testing.T) {
	service, root := localService(t)

	if err := os.MkdirAll(filepath.Join(root, "Show"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := service.LocalPath("Show/ep01.mkv"); err != nil {
		t.Errorf("an ordinary path should resolve: %v", err)
	}

	escapeAttempts := []string{
		"../../etc/passwd",
		"Show/../../../../etc/passwd",
		filepath.Join(root, "..", "..", "Windows", "System32", "drivers", "etc", "hosts"),
		"..",
		"../secret",
	}
	for _, attempt := range escapeAttempts {
		if resolved, err := service.LocalPath(attempt); err == nil {
			t.Errorf("LocalPath(%q) = %q, want an escape error", attempt, resolved)
		}
	}

	// A leading separator and an interior ".." that stays inside the root are normalized to
	// paths under the library rather than treated as escapes: "/etc/passwd" resolves to
	// <root>/etc/passwd, and "Show/.." resolves to the root itself. Both remain inside the
	// boundary, which is the property the callers depend on.
	for _, inRoot := range []string{"/etc/passwd", "Show/..", "./Show/ep01.mkv"} {
		resolved, err := service.LocalPath(inRoot)
		if err != nil {
			t.Errorf("LocalPath(%q) = %v, want it kept inside the root", inRoot, err)
			continue
		}
		if !withinRoot(root, resolved) {
			t.Errorf("LocalPath(%q) escaped to %q", inRoot, resolved)
		}
	}
}

// TestLocalPathFollowsLinksOnlyInsideRoot is the case lexical cleaning cannot catch: a
// symbolic link placed inside the library resolves somewhere else. Creating one needs
// privileges on Windows, so that platform is skipped rather than reported as a pass.
func TestLocalPathFollowsLinksOnlyInsideRoot(t *testing.T) {
	if runtime.GOOS == "windows" && os.Getenv("NUMBER_OF_PROCESSORS") != "" {
		// Symlinks on Windows require developer mode or elevation; the check is attempted
		// and skipped if the link could not be made.
		t.Log("attempting symlink creation on Windows; may be refused without developer mode")
	}
	service, root := localService(t)

	outside := t.TempDir()
	target := filepath.Join(outside, "secret.conf")
	if err := os.WriteFile(target, []byte("do not read"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(root, "Show")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}

	if _, err := service.LocalPath(filepath.Join("Show", "secret.conf")); err == nil {
		t.Error("a path that leaves the root through a symlink must be refused")
	}
}

func TestWithinRoot(t *testing.T) {
	root := filepath.Join("/mnt", "anime")
	cases := []struct {
		target string
		want   bool
	}{
		{filepath.Join(root, "Show", "ep.mkv"), true},
		{root, true},
		{filepath.Join(root, "..", "etc"), false},
		{filepath.Join(root, ".."), false},
		{"/etc/passwd", false},
		{filepath.Join("/mnt", "anime2", "x.mkv"), false},
	}
	for _, tc := range cases {
		if got := withinRoot(root, tc.target); got != tc.want {
			t.Errorf("withinRoot(%q, %q) = %v, want %v", root, tc.target, got, tc.want)
		}
	}
}

func TestIsVideoFileUsesConfiguredExtensions(t *testing.T) {
	service, _ := localService(t)

	for _, name := range []string{"a.mkv", "b.MP4", "c.webm", "Show/d.avi"} {
		if !service.IsVideoFile(name) {
			t.Errorf("%s should be indexed with the default extension list", name)
		}
	}
	for _, name := range []string{"a.srt", "a.vtt", "a.nfo", "noext", "a.flac", "a.mkvw"} {
		if service.IsVideoFile(name) {
			t.Errorf("%s must not be treated as a video", name)
		}
	}
}

func TestBucketNamePerMode(t *testing.T) {
	service, _ := localService(t)
	if service.Bucket() != "local" {
		t.Errorf("local mode bucket = %q, want the literal 'local' the library rows use", service.Bucket())
	}
	if service.Mode() != config.StorageLocal {
		t.Errorf("mode = %q", service.Mode())
	}
}

func TestAudioCacheDirIsSkippedByDiscovery(t *testing.T) {
	service, root := localService(t)

	if err := os.MkdirAll(filepath.Join(root, "Show", ".animind-audio-cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Show", ".animind-audio-cache", "cached.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Show", "real.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := service.ListVideoFiles(t.Context())
	if err != nil {
		t.Fatalf("ListVideoFiles: %v", err)
	}
	if len(files) != 1 || files[0] != "Show/real.mkv" {
		t.Errorf("listed %v; the derived audio cache must never be indexed", files)
	}
}

func TestWriteFileAtomicLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "English.vtt")

	if err := WriteFileAtomic(target, []byte("WEBVTT\n\n")); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "WEBVTT\n\n" {
		t.Errorf("content = %q", data)
	}
	// The temp file must be gone, not left beside the result.
	leftovers, _ := os.ReadDir(filepath.Dir(target))
	if len(leftovers) != 1 {
		t.Errorf("expected only the target file, found %d entries", len(leftovers))
	}
}

func TestGetObjectRejectsEscapingKey(t *testing.T) {
	service, _ := localService(t)
	if _, err := service.GetObject(t.Context(), "../../etc/passwd", ""); err == nil {
		t.Error("GetObject must refuse a key that leaves the root")
	}
}
