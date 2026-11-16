package agentkit

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestPackUnpackRoundTrip(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "SKILL.md"), skillMD("p", "x"))
	write(t, filepath.Join(src, "scripts", "run.sh"), "echo hi\n")
	os.Chmod(filepath.Join(src, "scripts", "run.sh"), 0o755)
	write(t, filepath.Join(src, ".venv", "lib", "huge.py"), "dependency")
	write(t, filepath.Join(src, ".git", "HEAD"), "ref")

	archive, digest, err := Pack(src)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := hashDir(src); digest != want {
		t.Fatalf("Pack digest %s != hashDir %s", digest, want)
	}
	again, _, _ := Pack(src)
	if !bytes.Equal(archive, again) {
		t.Fatal("packing must be deterministic")
	}

	dest := filepath.Join(t.TempDir(), "restored", "p")
	if err := Unpack(archive, dest, digest); err != nil {
		t.Fatal(err)
	}
	if got, _ := hashDir(dest); got != digest {
		t.Fatal("restored skill does not match")
	}
	if st, err := os.Stat(filepath.Join(dest, "scripts", "run.sh")); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("executable bit lost: %v %v", st.Mode(), err)
	}
	if st, _ := os.Stat(filepath.Join(dest, "SKILL.md")); st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("restored files must be private: %v", st.Mode())
	}
	for _, skipped := range []string{".venv", ".git"} {
		if _, err := os.Stat(filepath.Join(dest, skipped)); !os.IsNotExist(err) {
			t.Fatalf("%s must not be packed", skipped)
		}
	}

	// Unpacking again replaces the previous copy (stale files go away).
	write(t, filepath.Join(dest, "stale.txt"), "old")
	if err := Unpack(archive, dest, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "stale.txt")); !os.IsNotExist(err) {
		t.Fatal("a re-unpack must replace the folder")
	}
}

func tarOf(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		h := h
		body := []byte("x")
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write(body)
		}
	}
	tw.Close()
	return buf.Bytes()
}

func TestUnpackRejectsUnsafeArchives(t *testing.T) {
	reg := func(name string) tar.Header { return tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg} }
	for name, archive := range map[string][]byte{
		"parent escape":     tarOf(t, reg("SKILL.md"), reg("../evil")),
		"nested escape":     tarOf(t, reg("a/../../evil")),
		"absolute path":     tarOf(t, reg("/tmp/evil")),
		"dot element":       tarOf(t, reg("./SKILL.md")),
		"backslash":         tarOf(t, reg(`..\evil`)),
		"symlink":           tarOf(t, tar.Header{Name: "link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}),
		"hardlink":          tarOf(t, tar.Header{Name: "hard", Linkname: "SKILL.md", Typeflag: tar.TypeLink}),
		"directory entry":   tarOf(t, tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755}),
		"duplicate entries": tarOf(t, reg("SKILL.md"), reg("SKILL.md")),
		"not a tar":         []byte("garbage that is not a tar archive at all......................................................................................................................................................................................................................................................................................................................................................................................................................................................................................................................................................"),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "kit", "skill")
			err := Unpack(archive, dest, "irrelevant")
			if !errors.Is(err, ErrBadArchive) {
				t.Fatalf("accepted: %v", err)
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("destination created for a rejected archive")
			}
			// Nothing may be written anywhere, not even next to the destination:
			// path checks must reject an entry before any byte hits the disk.
			filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					t.Errorf("file written for a rejected archive: %s", p)
				}
				return nil
			})
		})
	}
}

func TestUnpackRejectsDigestMismatch(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "SKILL.md"), skillMD("p", "x"))
	archive, digest, _ := Pack(src)
	dest := filepath.Join(t.TempDir(), "p")
	write(t, filepath.Join(dest, "keep.txt"), "previous good copy")
	wrong := "0" + digest[1:]
	if err := Unpack(archive, dest, wrong); !errors.Is(err, ErrBadArchive) {
		t.Fatalf("mismatch accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "keep.txt")); err != nil {
		t.Fatal("a failed unpack must leave the previous copy in place")
	}
}
