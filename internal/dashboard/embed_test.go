package dashboard

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStandalone_BundleIsReal(t *testing.T) {
	// The committed standalone.tgz should be the real Next.js bundle
	// (built via `make dashboard-web-package`), not the NOT_BUILT
	// placeholder. The placeholder is only valid in a developer clone
	// before the first build.
	if StandaloneIsPlaceholder() {
		t.Fatal("StandaloneIsPlaceholder = true; the committed tarball should be the real Next.js bundle, not the NOT_BUILT placeholder")
	}
	if StandaloneSize() == 0 {
		t.Fatal("StandaloneSize = 0; the tarball appears empty")
	}
}

func TestStandaloneSHA256_Deterministic(t *testing.T) {
	a := StandaloneSHA256()
	b := StandaloneSHA256()
	if a != b {
		t.Fatalf("SHA256 not deterministic: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("SHA256 hex length = %d; want 64", len(a))
	}
}

func TestExtractTarball_RefusesPlaceholderWhenGuarded(t *testing.T) {
	// Build a synthetic placeholder tarball (mirrors what the NOT_BUILT
	// placeholder actually contains: a single "NOT_BUILT.txt" entry).
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	const content = "placeholder"
	hdr := &tar.Header{
		Name:     "NOT_BUILT.txt",
		Mode:     0o644,
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tw.Close()
	gz.Close()

	dest := t.TempDir()
	_, err := extractTarball(buf.Bytes(), dest, true /* placeholderGuard */)
	if err == nil {
		t.Fatal("extractTarball with placeholder+guard returned nil; want error")
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("error = %v; want 'placeholder'", err)
	}
}

// buildFixtureTarball synthesises a tarball with the given files.
// Keys are paths relative to the tarball root; values are file
// contents. Used by tests to exercise extraction without an actual
// Next.js build.
func buildFixtureTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractTarball_WritesFilesAndVersion(t *testing.T) {
	data := buildFixtureTarball(t, map[string]string{
		"server.js":      "console.log('hi')",
		"package.json":   `{"name":"fixture"}`,
		"pub/static.css": "body{}",
	})

	dest := t.TempDir()
	extracted, err := extractTarball(data, dest, false)
	if err != nil {
		t.Fatalf("extractTarball: %v", err)
	}
	if !extracted {
		t.Fatal("extracted = false on first extraction; want true")
	}

	for path, want := range map[string]string{
		"server.js":      "console.log('hi')",
		"package.json":   `{"name":"fixture"}`,
		"pub/static.css": "body{}",
	} {
		got, err := os.ReadFile(filepath.Join(dest, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s contents = %q; want %q", path, got, want)
		}
	}

	version := tarballSHA256(data)
	got, err := os.ReadFile(filepath.Join(dest, ".maquinista-version"))
	if err != nil {
		t.Fatalf("read version file: %v", err)
	}
	if string(got) != version {
		t.Errorf("version = %q; want %q", got, version)
	}
}

func TestExtractTarball_SecondCallIsNoOp(t *testing.T) {
	data := buildFixtureTarball(t, map[string]string{
		"a.txt": "alpha",
	})
	dest := t.TempDir()

	if _, err := extractTarball(data, dest, false); err != nil {
		t.Fatalf("first extract: %v", err)
	}

	// Mutate an extracted file — a no-op second call must not
	// overwrite it, confirming the cache hit path.
	if err := os.WriteFile(filepath.Join(dest, "a.txt"), []byte("mutated"), 0o644); err != nil {
		t.Fatalf("mutate: %v", err)
	}

	extracted, err := extractTarball(data, dest, false)
	if err != nil {
		t.Fatalf("second extract: %v", err)
	}
	if extracted {
		t.Fatal("extracted = true on second call with unchanged version; want false")
	}

	got, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	if err != nil {
		t.Fatalf("read after no-op: %v", err)
	}
	if string(got) != "mutated" {
		t.Fatalf("a.txt = %q; want %q (cache hit should not re-extract)", got, "mutated")
	}
}

func TestExtractTarball_VersionChangeRetriggers(t *testing.T) {
	v1 := buildFixtureTarball(t, map[string]string{"file.txt": "v1"})
	v2 := buildFixtureTarball(t, map[string]string{"file.txt": "v2"})
	dest := t.TempDir()

	if _, err := extractTarball(v1, dest, false); err != nil {
		t.Fatalf("v1 extract: %v", err)
	}
	if _, err := extractTarball(v2, dest, false); err != nil {
		t.Fatalf("v2 extract: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "file.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "v2" {
		t.Fatalf("file.txt = %q; want v2 (version change should re-extract)", got)
	}
}

func TestSafeJoin_RejectsTraversal(t *testing.T) {
	base := t.TempDir()
	cases := []string{"../evil", "../../etc/passwd", "/etc/passwd", "a/../../b"}
	for _, c := range cases {
		if _, err := safeJoin(base, c); err == nil {
			t.Errorf("safeJoin(%q) = nil; want traversal error", c)
		}
	}
}

func TestSafeJoin_AllowsInside(t *testing.T) {
	base := t.TempDir()
	joined, err := safeJoin(base, "a/b/c.txt")
	if err != nil {
		t.Fatalf("safeJoin: %v", err)
	}
	if !strings.HasPrefix(joined, base) {
		t.Fatalf("joined = %q; want prefix %q", joined, base)
	}
}

func TestExtractTarball_RejectsTraversalInTar(t *testing.T) {
	// A hand-built tarball with a malicious entry.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name:     "../evil.txt",
		Mode:     0o644,
		Size:     4,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tw.Close()
	gz.Close()

	dest := t.TempDir()
	_, err := extractTarball(buf.Bytes(), dest, false)
	if err == nil {
		t.Fatal("extractTarball accepted traversal entry; want error")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("error = %v; want 'escapes'", err)
	}
}

func TestExtractTarball_BadGzip(t *testing.T) {
	_, err := extractTarball([]byte("not-gzip"), t.TempDir(), false)
	if err == nil {
		t.Fatal("extractTarball on junk bytes returned nil; want error")
	}
	if !errors.Is(err, err) {
		// just to reference errors import
		t.Fatal("unreachable")
	}
}
