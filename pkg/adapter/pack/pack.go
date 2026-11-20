// Package pack unpacks adapter packages: tar or zip archives, optionally
// compressed with gzip, zstd, xz or bzip2. It tells the format from the
// contents.
package pack

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"pixelbrite.site/adapter"
)

const (
	// maxUnpacked bounds what a package unpacks to, against compression bombs.
	maxUnpacked = 4 << 30
	// maxEntries bounds how many entries a package holds.
	maxEntries = 100_000
)

// Download kinds recognized by their first bytes.
const (
	kindTar   = "tar"
	kindZip   = "zip"
	kindGzip  = "gzip"
	kindZstd  = "zstd"
	kindXz    = "xz"
	kindBzip2 = "bzip2"
)

// sniff names what head, a file's first bytes, begins.
func sniff(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		return kindGzip
	case bytes.HasPrefix(head, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return kindZstd
	case bytes.HasPrefix(head, []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}):
		return kindXz
	case bytes.HasPrefix(head, []byte("BZh")):
		return kindBzip2
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("PK\x05\x06")):
		return kindZip
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		return kindTar
	}
	return ""
}

func sniffFile(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	return sniff(head[:n]), nil
}

// IsPackage reports whether file begins as a package: a tar or zip archive,
// plain or compressed with gzip, zstd, xz or bzip2.
func IsPackage(file string) (bool, error) {
	kind, err := sniffFile(file)
	return kind != "", err
}

// Unpack extracts the package in file into dest, a folder it creates, and
// returns the adapter's folder inside it: dest itself, or the one top-level
// folder holding adapter.json. It recognizes the package by its content,
// decompresses it first when it is compressed, refuses entries and links
// that leave dest and device files, and stops when ctx ends.
func Unpack(ctx context.Context, file, dest string) (string, error) {
	if err := unpack(ctx, file, dest); err != nil {
		return "", err
	}
	return adapterFolder(dest)
}

func unpack(ctx context.Context, file, dest string) error {
	kind, err := sniffFile(file)
	if err != nil {
		return err
	}
	switch kind {
	case kindGzip, kindZstd, kindXz, kindBzip2:
		raw := file + ".raw"
		if err := decompress(ctx, file, raw, kind); err != nil {
			return err
		}
		defer os.Remove(raw)
		if kind, err = sniffFile(raw); err != nil {
			return err
		}
		file = raw
	}
	if kind == "" {
		return errors.New("the file is not a tar or zip package (plain, gzip, zstd, xz or bzip2)")
	}
	if err := os.Mkdir(dest, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()
	switch kind {
	case kindTar:
		err = untar(ctx, file, root)
	case kindZip:
		err = unzip(ctx, file, root)
	default:
		return errors.New("the download is not a tar or zip package")
	}
	if err != nil {
		return err
	}
	return checkLinks(dest, root)
}

// checkLinks resolves every link in the unpacked package through root, which
// refuses a chain of links that ends outside it.
func checkLinks(dest string, root *os.Root) error {
	return filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		rel, err := filepath.Rel(dest, p)
		if err != nil {
			return err
		}
		if _, err := root.Stat(rel); err != nil {
			return fmt.Errorf("the package link %q points outside the package or to nothing", filepath.ToSlash(rel))
		}
		return nil
	})
}

// ctxReader stops a read when ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}

func decompress(ctx context.Context, file, dest, kind string) error {
	in, err := os.Open(file)
	if err != nil {
		return err
	}
	defer in.Close()
	var r io.Reader
	switch kind {
	case kindGzip:
		gz, err := gzip.NewReader(in)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	case kindZstd:
		zr, err := zstd.NewReader(in)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	case kindXz:
		if r, err = xz.NewReader(in); err != nil {
			return err
		}
	case kindBzip2:
		r = bzip2.NewReader(in)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(ctxReader{ctx, r}, maxUnpacked+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("decompressing the %s package: %w", kind, err)
	}
	if n > maxUnpacked {
		return errors.New("the package unpacks to more than 4 GiB")
	}
	return nil
}

// entryName is a package entry's path inside the package, "." for the
// package's own folder, or an error when it would leave it.
func entryName(name string) (string, error) {
	slashed := strings.ReplaceAll(name, `\`, "/")
	clean := path.Clean(slashed)
	if slashed == "" || path.IsAbs(slashed) || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(slashed, ":") {
		return "", fmt.Errorf("the package entry %q leaves the package", name)
	}
	return clean, nil
}

// skipped reports an entry that carries no file of the package: macOS's
// AppleDouble files (._name), which its tar adds for extended attributes,
// and its __MACOSX folder in zip archives.
func skipped(name string) bool {
	return strings.HasPrefix(path.Base(name), "._") || name == "__MACOSX" || strings.HasPrefix(name, "__MACOSX/")
}

// counter counts entries against maxEntries and stops when ctx ends.
type counter struct {
	ctx context.Context
	n   int
}

func (c *counter) next() error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if c.n++; c.n > maxEntries {
		return fmt.Errorf("the package holds more than %d entries", maxEntries)
	}
	return nil
}

// linkTarget checks that a link at name pointing to target stays inside the package.
func linkTarget(name, target string) error {
	if path.IsAbs(target) || strings.Contains(target, `\`) {
		return fmt.Errorf("the package link %q points outside the package", name)
	}
	resolved := path.Join(path.Dir(name), target)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("the package link %q points outside the package", name)
	}
	return nil
}

// budget counts unpacked bytes against maxUnpacked.
type budget struct{ left int64 }

func (b *budget) copy(w io.Writer, r io.Reader) error {
	n, err := io.Copy(w, io.LimitReader(r, b.left+1))
	b.left -= n
	if err == nil && b.left < 0 {
		err = errors.New("the package unpacks to more than 4 GiB")
	}
	return err
}

func writeEntry(root *os.Root, name string, mode fs.FileMode, r io.Reader, b *budget) error {
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm()|0o600)
	if err != nil {
		return err
	}
	err = b.copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func untar(ctx context.Context, file string, root *os.Root) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	b := &budget{left: maxUnpacked}
	count := &counter{ctx: ctx}
	archive := tar.NewReader(f)
	for {
		h, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the package: %w", err)
		}
		if err := count.next(); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader { // git archive's commit ID; no file
			continue
		}
		name, err := entryName(h.Name)
		if err != nil {
			return err
		}
		if name == "." || skipped(name) {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, 0o755)
		case tar.TypeReg:
			err = writeEntry(root, name, fs.FileMode(h.Mode), archive, b)
		case tar.TypeSymlink:
			if err = linkTarget(name, h.Linkname); err == nil {
				if err = root.MkdirAll(path.Dir(name), 0o755); err == nil {
					err = root.Symlink(h.Linkname, name)
				}
			}
		case tar.TypeLink:
			var target string
			if target, err = entryName(h.Linkname); err == nil {
				err = root.Link(target, name)
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return fmt.Errorf("the package entry %q is a device file", h.Name)
		default:
			return fmt.Errorf("the package entry %q has a type the server does not unpack", h.Name)
		}
		if err != nil {
			return fmt.Errorf("unpacking %q: %w", h.Name, err)
		}
	}
}

func unzip(ctx context.Context, file string, root *os.Root) error {
	archive, err := zip.OpenReader(file)
	if err != nil {
		return fmt.Errorf("reading the package: %w", err)
	}
	defer archive.Close()
	b := &budget{left: maxUnpacked}
	count := &counter{ctx: ctx}
	for _, entry := range archive.File {
		if err := count.next(); err != nil {
			return err
		}
		name, err := entryName(entry.Name)
		if err != nil {
			return err
		}
		if name == "." || skipped(name) {
			continue
		}
		mode := entry.Mode()
		switch {
		case mode.IsDir():
			err = root.MkdirAll(name, 0o755)
		case mode&fs.ModeSymlink != 0:
			err = unzipLink(root, entry, name)
		case mode.Type() != 0:
			return fmt.Errorf("the package entry %q is a device file", entry.Name)
		default:
			var rc io.ReadCloser
			if rc, err = entry.Open(); err == nil {
				err = writeEntry(root, name, mode, rc, b)
				_ = rc.Close()
			}
		}
		if err != nil {
			return fmt.Errorf("unpacking %q: %w", entry.Name, err)
		}
	}
	return nil
}

func unzipLink(root *os.Root, entry *zip.File, name string) error {
	rc, err := entry.Open()
	if err != nil {
		return err
	}
	target, err := io.ReadAll(io.LimitReader(rc, 4096))
	_ = rc.Close()
	if err != nil {
		return err
	}
	if err := linkTarget(name, string(target)); err != nil {
		return err
	}
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	return root.Symlink(string(target), name)
}

// adapterFolder finds adapter.json at the top of the package or inside its
// one top-level folder.
func adapterFolder(staging string) (string, error) {
	if _, err := os.Stat(filepath.Join(staging, adapter.ManifestName)); err == nil {
		return staging, nil
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return "", err
	}
	var folders []string
	for _, e := range entries {
		if e.Name() == ".DS_Store" {
			continue
		}
		folders = append(folders, e.Name())
	}
	if len(folders) == 1 {
		dir := filepath.Join(staging, folders[0])
		if info, err := os.Lstat(dir); err == nil && info.IsDir() {
			if _, err := os.Stat(filepath.Join(dir, adapter.ManifestName)); err == nil {
				return dir, nil
			}
		}
	}
	return "", fmt.Errorf("the package has no %s at its top or in one top-level folder", adapter.ManifestName)
}

// MarkPrograms sets the execute bits of each program m names in dir, which
// zip archives made on Windows do not record.
func MarkPrograms(dir string, m adapter.Manifest) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, rel := range m.Programs {
		info, err := root.Stat(rel)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := root.Chmod(rel, info.Mode().Perm()|0o755); err != nil {
			return err
		}
	}
	return nil
}
