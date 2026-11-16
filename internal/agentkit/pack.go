package agentkit

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Pack archives a skill folder as an uncompressed tar of exactly the files
// that make up its fingerprint, and returns that fingerprint computed from
// the packed bytes themselves (so a file edited mid-backup cannot produce an
// archive that disagrees with its digest). Entries carry no owner or time,
// and only the executable bit of the mode: the same skill packs identically
// on every machine.
func Pack(dir string) ([]byte, string, error) {
	files, err := skillFiles(dir)
	if err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	h := sha256.New()
	for _, f := range files {
		data, err := os.ReadFile(f.abs)
		if err != nil {
			return nil, "", err
		}
		mode := int64(0o644)
		if f.exec {
			mode = 0o755
		}
		hdr := &tar.Header{Name: f.rel, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, "", err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, "", err
		}
		digestEntry(h, f.rel, f.exec, int64(len(data)))
		h.Write(data)
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), hex.EncodeToString(h.Sum(nil)), nil
}

var ErrBadArchive = errors.New("invalid skill archive")

// Unpack extracts a Pack archive into dest, replacing it, and verifies that
// the result has the expected digest. Archives come from the vault, which
// is authenticated, but they are still treated as untrusted: only regular
// files with clean relative paths are accepted, within the same limits as
// a live skill. Nothing is written to dest unless the whole archive checks out.
func Unpack(archive []byte, dest, wantDigest string) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".unpack-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	tr := tar.NewReader(bytes.NewReader(archive))
	var (
		count int
		total int64
	)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrBadArchive, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("%w: %q is not a regular file", ErrBadArchive, hdr.Name)
		}
		name, ok := cleanRel(hdr.Name)
		if !ok {
			return fmt.Errorf("%w: unsafe path %q", ErrBadArchive, hdr.Name)
		}
		if count++; count > maxSkillFiles {
			return fmt.Errorf("%w: more than %d files", ErrBadArchive, maxSkillFiles)
		}
		if total += hdr.Size; hdr.Size < 0 || total > maxSkillBytes {
			return fmt.Errorf("%w: larger than %d MiB", ErrBadArchive, maxSkillBytes>>20)
		}
		target := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if hdr.Mode&0o111 != 0 {
			mode = 0o700
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrBadArchive, err) // e.g. a duplicate entry
		}
		_, err = io.CopyN(out, tr, hdr.Size)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}

	got, err := hashDir(tmp)
	if err != nil {
		return err
	}
	if got != wantDigest {
		return fmt.Errorf("%w: digest %s, want %s", ErrBadArchive, got[:12], short(wantDigest))
	}
	// Swap into place: the previous copy (if any) is only removed once the new one is there.
	old := ""
	if _, err := os.Lstat(dest); err == nil {
		old = tmp + ".old"
		if err := os.Rename(dest, old); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		if old != "" {
			_ = os.Rename(old, dest)
		}
		return err
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}
	return nil
}

// cleanRel accepts only relative, slash-separated paths that stay inside the
// destination and contain no empty, "." or ".." element.
func cleanRel(name string) (string, bool) {
	if name == "" || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	return name, true
}

func short(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
