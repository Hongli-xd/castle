package oci

// File operations over the Docker archive endpoints (PUT/GET …/archive),
// which speak a single-member tar. Directory listing goes through a shell
// exec because the archive API is recursive — fine for one file, wasteful
// for a listing. Sandbox base images are required to ship /bin/sh (ours do).

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"strings"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/runtime"
)

const archiveOpTimeout = 60 * time.Second

// PutFile writes one file at an absolute in-sandbox path.
func (b *Backend) PutFile(ctx context.Context, ref string, req runtime.PutFileRequest) error {
	ctx, cancel := context.WithTimeout(ctx, archiveOpTimeout)
	defer cancel()
	p := req.Path
	if !strings.HasPrefix(p, "/") {
		return apperr.Invalid("file path must be absolute: %q", p)
	}
	if strings.HasSuffix(p, "/") {
		return apperr.Invalid("file path must name a file, not a directory")
	}
	mode := req.Mode
	if mode == 0 {
		mode = 0o644
	}
	dir := parentDir(p)
	base := baseName(p)

	// The archive API requires the destination directory to exist; it does
	// not create parents. Provision them explicitly (idempotent).
	res, err := b.Exec(ctx, ref, runtime.ExecRequest{
		Argv:    []string{"/bin/sh", "-c", "mkdir -p -- \"$1\"", "mkdir", dir},
		Timeout: 15 * time.Second,
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return apperr.ConflictErr("mkdir %s: %s", dir, strings.TrimSpace(string(res.Stderr)))
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name:     base,
		Mode:     int64(mode),
		Size:     int64(len(req.Content)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Now().UTC(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return apperr.InternalErr("tar header: %v", err)
	}
	if _, err := tw.Write(req.Content); err != nil {
		return apperr.InternalErr("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		return apperr.InternalErr("tar close: %v", err)
	}
	resp, closer, err := b.c.raw(ctx, "PUT",
		"/containers/"+urlQueryEscape(ref)+"/archive", map[string]string{"path": dir}, &buf,
		map[string]string{"Content-Type": "application/x-tar"})
	if err != nil {
		return err
	}
	defer closer()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, respLimit))
		return statusErr(resp.StatusCode, "archive put", body)
	}
	return nil
}

// GetFile reads one file, capped at maxSize. Oversized files fail with a
// quota error instead of being silently truncated — partial executables are
// worse than an explicit error.
func (b *Backend) GetFile(ctx context.Context, ref, path string, maxSize int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, archiveOpTimeout)
	defer cancel()
	if !strings.HasPrefix(path, "/") {
		return nil, apperr.Invalid("file path must be absolute: %q", path)
	}
	if maxSize <= 0 {
		maxSize = 100 << 20
	}
	resp, closer, err := b.c.raw(ctx, "GET",
		"/containers/"+urlQueryEscape(ref)+"/archive", map[string]string{"path": path}, nil, nil)
	if err != nil {
		return nil, err
	}
	defer closer()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, respLimit))
		return nil, statusErr(resp.StatusCode, "archive get", body)
	}
	tr := tar.NewReader(io.LimitReader(resp.Body, maxSize+512<<10))
	hdr, err := tr.Next()
	if err != nil {
		return nil, apperr.Unavailable("docker archive get %s: %v", path, err)
	}
	if hdr.Size > maxSize {
		return nil, apperr.Quota("file %s is %d bytes, above the %d byte retrieval cap", path, hdr.Size, maxSize)
	}
	out := make([]byte, hdr.Size)
	if _, err := io.ReadFull(tr, out); err != nil {
		return nil, apperr.Unavailable("read %s from archive: %v", path, err)
	}
	return out, nil
}

// ListDir lists one directory level via /bin/sh. Names containing newlines
// are unsupported (upload validation rejects control characters).
func (b *Backend) ListDir(ctx context.Context, ref, path string) ([]runtime.FileInfo, error) {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return nil, apperr.Invalid("invalid directory path %q", path)
	}
	res, err := b.Exec(ctx, ref, runtime.ExecRequest{
		Argv:    []string{"/bin/sh", "-c", `for p in "$1"/* "$1"/.[!.]*; do if [ -e "$p" ]; then if [ -d "$p" ]; then printf 'd %s\n' "$p"; else printf 'f %s\n' "$p"; fi; fi; done`, "ls", path},
		Timeout: 20 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, apperr.ConflictErr("list %s: %s", path, strings.TrimSpace(string(res.Stderr)))
	}
	out := []runtime.FileInfo{}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		isDir := false
		if len(line) > 2 && line[1] == ' ' {
			isDir = line[0] == 'd'
			line = line[2:]
		}
		name := baseName(line)
		if name == "" {
			continue
		}
		out = append(out, runtime.FileInfo{Name: name, IsDir: isDir})
	}
	return out, nil
}

// DeletePath removes a file or directory tree via the container's shell.
func (b *Backend) DeletePath(ctx context.Context, ref, path string) error {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") || path == "/" {
		return apperr.Invalid("refusing to delete invalid or root path %q", path)
	}
	res, err := b.Exec(ctx, ref, runtime.ExecRequest{
		Argv:    []string{"/bin/sh", "-c", "rm -rf -- \"$1\"", "rm", path},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return apperr.ConflictErr("delete %s: %s", path, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

func parentDir(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func baseName(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
