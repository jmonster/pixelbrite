package pack

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUnpackRefusesEntriesThatLeaveThePackage(t *testing.T) {
	outside := t.TempDir()
	cases := map[string]func(*tar.Writer){
		"parent":        func(w *tar.Writer) { reg(w, "../escape", "x") },
		"absolute":      func(w *tar.Writer) { reg(w, filepath.Join(outside, "abs"), "x") },
		"link outside":  func(w *tar.Writer) { link(w, "a/out", "../../escape") },
		"link absolute": func(w *tar.Writer) { link(w, "out", outside) },
		"through link": func(w *tar.Writer) {
			link(w, "dir", ".")
			reg(w, "dir/../../escape", "x")
		},
		"link through link": func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Name: "a/b/", Typeflag: tar.TypeDir, Mode: 0o755, Format: tar.FormatPAX})
			link(w, "a/up", "b/..")        // a
			link(w, "a/b/far", "../../..") // lexically outside: refused
			link(w, "deep", "a/b")
			link(w, "out", "deep/../..") // lexically "." but deep is a/b, so outside
		},
		"device": func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Name: "dev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3, Format: tar.FormatUSTAR})
		},
	}
	for name, write := range cases {
		var buf bytes.Buffer
		w := tar.NewWriter(&buf)
		write(w)
		_ = w.Close()
		file := filepath.Join(t.TempDir(), "pkg")
		if err := os.WriteFile(file, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Unpack(context.Background(), file, filepath.Join(t.TempDir(), "out")); err == nil {
			t.Errorf("%s: unpacked", name)
		}
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("../escape")
	_, _ = f.Write([]byte("x"))
	_ = zw.Close()
	file := filepath.Join(t.TempDir(), "pkg.zip")
	_ = os.WriteFile(file, buf.Bytes(), 0o600)
	if _, err := Unpack(context.Background(), file, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Error("zip with a parent entry unpacked")
	}
	if entries, _ := os.ReadDir(outside); len(entries) > 0 {
		t.Errorf("unpacking wrote outside the package: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(outside), "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Error("unpacking wrote beside the package")
	}
}

func reg(w *tar.Writer, name, body string) {
	_ = w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg, Format: tar.FormatPAX})
	_, _ = w.Write([]byte(body))
}

func link(w *tar.Writer, name, target string) {
	_ = w.WriteHeader(&tar.Header{Name: name, Linkname: target, Typeflag: tar.TypeSymlink, Format: tar.FormatPAX})
}
