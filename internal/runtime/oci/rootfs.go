package oci

// Offline rootfs import: registries are often unreachable in enterprise and
// air-gapped networks, so building a base image from a local rootfs directory
// is a first-class product capability, not a workaround.
//
// The builder reproduces the host's merged-/usr layout (Ubuntu 24.04 and
// Arch keep /bin, /lib, /lib64 as symlinks into usr/*). Naively creating
// real directories there puts binaries in /usr/bin while /bin stays empty
// and the image cannot boot; blindly "de-duplicating" symlinks with
// os.SameFile also collapses /usr/bin/sh into /usr/bin/dash and deletes the
// one link that makes the image usable. Both failure modes shipped in an
// earlier iteration; the normalization below is the fix.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
)

// BuildRootfsTar walks rootfsDir and produces an importable image tar.
// The directory must already contain a complete root filesystem.
func BuildRootfsTar(rootfsDir string) ([]byte, error) {
	info, err := os.Stat(rootfsDir)
	if err != nil {
		return nil, apperr.Invalid("rootfs dir: %v", err)
	}
	if !info.IsDir() {
		return nil, apperr.Invalid("rootfs path %q is not a directory", rootfsDir)
	}
	// a bootable userspace needs at least these
	for _, must := range []string{"usr/bin"} {
		if fi, err := os.Stat(filepath.Join(rootfsDir, must)); err != nil || !fi.IsDir() {
			return nil, apperr.Invalid("rootfs %q lacks %s — not a usable root filesystem", rootfsDir, must)
		}
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	base := filepath.Clean(rootfsDir)

	//顶层重写规则：把历史上建在根下的实体目录归一化为 usr/ 下的真身，
	//bin/lib/lib64 一律作为指向 usr/* 的符号链接写入（merged-/usr 布局）。
	rewrite := map[string]string{
		"bin":   "usr/bin",
		"lib":   "usr/lib",
		"lib64": "usr/lib64",
		"sbin":  "usr/sbin",
	}
	seenTop := map[string]bool{}

	err = filepath.Walk(base, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		// first component handling
		top := rel
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			top = rel[:i]
		}
		if dst, ok := rewrite[top]; ok {
			if rel == top {
				return nil // skip the real dir; we synthesize the symlink below
			}
			rel = dst + strings.TrimPrefix(rel, top)
		}
		if !strings.Contains(rel, "/") {
			if seenTop[rel] {
				return nil
			}
			seenTop[rel] = true
		}

		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		hdr.Format = tar.FormatPAX
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			hdr.Linkname = target
			hdr.Typeflag = tar.TypeSymlink
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() && hdr.Size > 0 {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, apperr.InternalErr("walk rootfs: %v", err)
	}

	// synthesize merged-/usr top-level symlinks
	for _, name := range []string{"bin", "lib", "lib64", "sbin"} {
		if seenTop[name] {
			continue
		}
		hdr := &tar.Header{
			Name:     name,
			Typeflag: tar.TypeSymlink,
			Linkname: "usr/" + name,
			Mode:     0o777,
			ModTime:  time.Now().UTC(),
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, apperr.InternalErr("tar symlink %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, apperr.InternalErr("tar close: %v", err)
	}
	return buf.Bytes(), nil
}

// ImportRootfs imports a rootfs directory as image tag.
func (b *Backend) ImportRootfs(ctx context.Context, rootfsDir, tag string) (string, int64, error) {
	tarball, err := BuildRootfsTar(rootfsDir)
	if err != nil {
		return "", 0, err
	}
	body, err := b.c.do(ctx, "POST",
		"/images/create", map[string]string{"fromSrc": "-", "repo": tag}, bytes.NewReader(tarball))
	if err != nil {
		return "", 0, err
	}
	_ = body
	size, err := b.imageSize(ctx, tag)
	if err != nil {
		return tag, 0, err
	}
	return tag, size, nil
}

// ImportRootfsTar imports a pre-built rootfs tar (optionally gzipped) as
// image tag — the air-gapped deployment path: rootfs tarballs are released
// as artifacts alongside the binary.
func (b *Backend) ImportRootfsTar(ctx context.Context, tarball []byte, gzipped bool, tag string) (string, int64, error) {
	payload := tarball
	if gzipped {
		zr, err := gzip.NewReader(bytes.NewReader(tarball))
		if err != nil {
			return "", 0, apperr.Invalid("rootfs tarball is not valid gzip: %v", err)
		}
		raw, err := io.ReadAll(zr)
		if err != nil {
			return "", 0, apperr.Invalid("rootfs tarball decompress: %v", err)
		}
		payload = raw
	}
	if _, err := b.c.do(ctx, "POST", "/images/create",
		map[string]string{"fromSrc": "-", "repo": tag}, bytes.NewReader(payload)); err != nil {
		return "", 0, err
	}
	size, err := b.imageSize(ctx, tag)
	if err != nil {
		return tag, 0, err
	}
	return tag, size, nil
}
