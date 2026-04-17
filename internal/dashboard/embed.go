package dashboard

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// standaloneTarball holds the gzipped tarball of the Next.js
// .next/standalone bundle. It's populated at build time by
// `make dashboard-web-package`; the committed version is a tiny
// placeholder (NOT_BUILT.txt) so `go build` succeeds without a
// prior npm build.
//
// Commit 1.5 introduces this wiring; Commit 1.6 teaches the
// supervisor to extract it at dashboard-start.

//go:embed standalone.tgz
var standaloneTarball []byte

// StandaloneIsPlaceholder reports whether the embedded tarball is
// the NOT_BUILT placeholder. Used by the supervisor to fail fast
// with a helpful message rather than silently running the
// placeholder on production boots.
func StandaloneIsPlaceholder() bool {
	return tarballIsPlaceholder(standaloneTarball)
}

// StandaloneSHA256 returns the SHA-256 of the embedded tarball in
// hex, suitable for naming a version directory so different
// maquinista builds extract to different directories.
func StandaloneSHA256() string {
	return tarballSHA256(standaloneTarball)
}

// StandaloneSize returns the size of the embedded tarball in bytes.
func StandaloneSize() int { return len(standaloneTarball) }

// tarballIsPlaceholder / tarballSHA256 split the core scanning out
// so tests can exercise them against a synthesised tarball without
// mutating the `//go:embed`ed byte slice.
func tarballIsPlaceholder(data []byte) bool {
	gz, err := gzip.NewReader(bytesReader(data))
	if err != nil {
		return false
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		return false
	}
	return hdr.Name == "NOT_BUILT.txt"
}

func tarballSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ExtractStandalone extracts the embedded tarball into dest.
// Behaviour:
//
//   - Creates dest with 0o755 if missing.
//   - Skips extraction and returns (false, nil) if dest already
//     contains a file named ".maquinista-version" whose contents
//     match the current tarball's SHA-256. This lets `dashboard
//     start` be fast on warm caches.
//   - On first extraction (or a version mismatch), writes files
//     with their tarball permissions; creates intermediate
//     directories as needed; rejects paths that escape dest
//     (tar-slip defence).
//   - After a successful extraction, writes the version file so
//     the next start is a no-op.
//
// Returns (extracted, err). extracted=true means new files were
// written; false means the cache was up-to-date.
func ExtractStandalone(dest string) (bool, error) {
	return extractTarball(standaloneTarball, dest, true)
}

// extractTarball is the reusable core. placeholderGuard=true makes
// it error when given the NOT_BUILT placeholder; tests pass false
// to exercise the extraction path against synthetic bundles.
func extractTarball(data []byte, dest string, placeholderGuard bool) (bool, error) {
	if placeholderGuard && tarballIsPlaceholder(data) {
		return false, errors.New("dashboard: embedded bundle is a placeholder; run `make dashboard-web-package` before releasing")
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", dest, err)
	}

	version := tarballSHA256(data)
	versionFile := filepath.Join(dest, ".maquinista-version")
	if existing, err := os.ReadFile(versionFile); err == nil && string(existing) == version {
		return false, nil
	}

	gz, err := gzip.NewReader(bytesReader(data))
	if err != nil {
		return false, fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, fmt.Errorf("tar read: %w", err)
		}

		// Reject absolute paths and '..' traversal.
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return false, err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode).Perm()|0o700); err != nil {
				return false, fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return false, fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode).Perm()|0o600)
			if err != nil {
				return false, fmt.Errorf("open %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return false, fmt.Errorf("write %s: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return false, fmt.Errorf("close %s: %w", target, err)
			}
		case tar.TypeSymlink:
			// Symlinks are rare in Next.js standalone output (node
			// deps are copied, not linked) but we tolerate them.
			// Point the link at its recorded target relative to
			// the target's parent dir, not into dest — the
			// standalone bundle is self-contained so relative
			// symlinks stay valid.
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return false, fmt.Errorf("symlink %s -> %s: %w", target, hdr.Linkname, err)
			}
		default:
			// Other tar types (block/char/fifo) have no place in
			// a web bundle. Skip quietly.
			continue
		}
	}

	if err := os.WriteFile(versionFile, []byte(version), 0o644); err != nil {
		return false, fmt.Errorf("write version file: %w", err)
	}
	return true, nil
}

// safeJoin returns filepath.Join(base, rel) after asserting the
// result stays under base. Guards against tar-slip attacks.
func safeJoin(base, rel string) (string, error) {
	cleaned := filepath.Clean(rel)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("tar entry escapes dest: %q", rel)
	}
	joined := filepath.Join(base, cleaned)
	// Double-check: joined must have base as a prefix.
	rp, err := filepath.Rel(base, joined)
	if err != nil || strings.HasPrefix(rp, "..") {
		return "", fmt.Errorf("tar entry escapes dest: %q", rel)
	}
	return joined, nil
}

// bytesReader is a small helper that returns an io.Reader backed
// by a []byte slice, keeping the file body in memory for
// predictable performance (tarball is ~50 MiB at most).
func bytesReader(b []byte) *byteSliceReader {
	return &byteSliceReader{b: b}
}

type byteSliceReader struct {
	b []byte
	n int
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.n >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.n:])
	r.n += n
	return n, nil
}
