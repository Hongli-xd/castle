package oci

// Integration tests against a real Docker daemon. They are skipped (not
// failed) when no daemon is reachable or when -short is passed, so unit CI
// stays green on machines without containers. Every case here exists because
// it caught, or guards against, a bug only a real container exposes.

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/runtime"
)

const testBaseImage = "sandboxd-test-base:alpine322"

func needDocker(t *testing.T) *Backend {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	c, err := NewClient("")
	if err != nil {
		t.Skipf("no docker daemon: %v", err)
	}
	b := New(c)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Ping(ctx); err != nil {
		t.Skipf("docker ping failed: %v", err)
	}
	// ensure the tiny test base image exists (pulled once per node)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel2()
	ok, err := b.HasImage(ctx2, testBaseImage)
	if err != nil {
		t.Fatalf("image check: %v", err)
	}
	if !ok {
		if _, err := b.c.do(ctx2, "POST", "/images/create",
			map[string]string{"fromImage": "docker.io/library/alpine", "tag": "3.22"}, nil); err != nil {
			t.Skipf("cannot pull test base image (offline node?): %v", err)
		}
		if _, err := b.c.do(ctx2, "POST", "/images/"+urlQueryEscape("alpine:3.22")+"/tag",
			map[string]string{"repo": testBaseImage}, nil); err != nil {
			t.Fatalf("tag base image: %v", err)
		}
	}
	return b
}

func mkSandbox(t *testing.T, b *Backend, memMiB int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	name := fmt.Sprintf("sbx-it-%d", time.Now().UnixNano()%1_000_000_000)
	ref, err := b.Create(ctx, runtime.CreateRequest{
		Name:          name,
		Image:         testBaseImage,
		CPUMillicores: 500,
		MemoryMiB:     memMiB,
		Labels:        map[string]string{"test": "oci-integration"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := b.Start(ctx, ref); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		cx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = b.Remove(cx, ref)
	})
	return ref
}

func TestLifecycleAndExec(t *testing.T) {
	b := needDocker(t)
	ref := mkSandbox(t, b, 128)
	ctx := context.Background()

	res, err := b.Exec(ctx, ref, runtime.ExecRequest{
		Argv:    []string{"/bin/echo", "hello", "sandboxd"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != "hello sandboxd" {
		t.Fatalf("exec result: code=%d out=%q err=%q", res.ExitCode, res.Stdout, res.Stderr)
	}

	// non-zero exit codes must surface, not be swallowed
	res, err = b.Exec(ctx, ref, runtime.ExecRequest{
		Argv:    []string{"/bin/sh", "-c", "exit 42"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 42 {
		t.Fatalf("exit code = %d, want 42", res.ExitCode)
	}

	// stderr separation
	res, _ = b.Exec(ctx, ref, runtime.ExecRequest{
		Argv:    []string{"/bin/sh", "-c", "echo out; echo err 1>&2"},
		Timeout: 30 * time.Second,
	})
	if strings.TrimSpace(string(res.Stdout)) != "out" || strings.TrimSpace(string(res.Stderr)) != "err" {
		t.Fatalf("stream demux broken: out=%q err=%q", res.Stdout, res.Stderr)
	}
}

func TestExecTimeoutKills(t *testing.T) {
	b := needDocker(t)
	ref := mkSandbox(t, b, 128)
	start := time.Now()
	res, err := b.Exec(context.Background(), ref, runtime.ExecRequest{
		Argv:    []string{"/bin/sleep", "30"},
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("timeout honored too slowly: %s", elapsed)
	}
}

func TestMemoryLimitOOMSurfaces(t *testing.T) {
	b := needDocker(t)
	ref := mkSandbox(t, b, 64)
	res, err := b.Exec(context.Background(), ref, runtime.ExecRequest{
		// exponential string growth lives entirely in the heap; command
		// substitution probes are invalid because busybox ash spills those
		// to a temp file (that flaw hid a passing limit as a failing one)
		Argv:      []string{"/bin/sh", "-c", `A=x; i=0; while [ $i -lt 200 ]; do A=$A$A; i=$((i+1)); done; echo survived`},
		Timeout:   90 * time.Second,
		MaxOutput: 4096,
	})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatalf("heap growth survived a 64MB cap: %+v", res)
	}
	t.Logf("OOM guard: exit=%d stderr=%q", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
}

func TestFileOpsRoundTrip(t *testing.T) {
	b := needDocker(t)
	ref := mkSandbox(t, b, 128)
	ctx := context.Background()

	payload := []byte("sandboxd file roundtrip test payload \x00\x01\x02\n")
	if err := b.PutFile(ctx, ref, runtime.PutFileRequest{
		Path: "/tmp/data/proof.txt", Content: payload, Mode: 0o600,
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := b.GetFile(ctx, ref, "/tmp/data/proof.txt", 1<<20)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("roundtrip mismatch: %q", got)
	}

	files, err := b.ListDir(ctx, ref, "/tmp/data")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 1 || files[0].Name != "proof.txt" || files[0].IsDir {
		t.Fatalf("list = %+v", files)
	}

	if err := b.DeletePath(ctx, ref, "/tmp/data/proof.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := b.GetFile(ctx, ref, "/tmp/data/proof.txt", 1<<20); err == nil {
		t.Fatal("file survived deletion")
	}

	if err := b.PutFile(ctx, ref, runtime.PutFileRequest{Path: "relative/evil", Content: []byte("x")}); err == nil {
		t.Fatal("relative path accepted")
	}
}

func TestPauseResume(t *testing.T) {
	b := needDocker(t)
	ref := mkSandbox(t, b, 128)
	ctx := context.Background()

	if err := b.Pause(ctx, ref, runtime.PauseOptions{MemoryBytes: 128 << 20}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	st, err := b.Inspect(ctx, ref)
	if err != nil || !st.Paused {
		t.Fatalf("inspect after pause: %+v err=%v", st, err)
	}
	// idempotent pause by real phase
	if err := b.Pause(ctx, ref, runtime.PauseOptions{MemoryBytes: 128 << 20}); err != nil {
		t.Fatalf("double pause should be a no-op: %v", err)
	}
	if err := b.Resume(ctx, ref, runtime.PauseOptions{MemoryBytes: 128 << 20}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	st, _ = b.Inspect(ctx, ref)
	if st.Paused || !st.Running {
		t.Fatalf("inspect after resume: %+v", st)
	}
	// the pause-time reclaim must have restored the original memory limit
	ins, err := b.inspect(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if ins.HostConfig.Memory != 128<<20 || ins.HostConfig.MemorySwap != 128<<20 {
		t.Fatalf("pause reclaim did not restore limits: mem=%d swap=%d", ins.HostConfig.Memory, ins.HostConfig.MemorySwap)
	}
	if err := b.Resume(ctx, ref, runtime.PauseOptions{MemoryBytes: 128 << 20}); err != nil {
		t.Fatalf("double resume should be a no-op: %v", err)
	}
	res, err := b.Exec(ctx, ref, runtime.ExecRequest{Argv: []string{"/bin/echo", "alive"}, Timeout: 30 * time.Second})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("exec after resume failed: %+v err=%v", res, err)
	}
}

func TestSnapshotCommitAndRestore(t *testing.T) {
	b := needDocker(t)
	ctx := context.Background()
	ref := mkSandbox(t, b, 128)

	if err := b.PutFile(ctx, ref, runtime.PutFileRequest{
		Path: "/root/state.txt", Content: []byte("state-before-snapshot"),
	}); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("sandboxd-test-snap:%d", time.Now().UnixNano()%1_000_000_000)
	imageRef, size, err := b.Commit(ctx, ref, tag)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if size <= 0 {
		t.Fatalf("committed image size = %d", size)
	}
	ok, err := b.HasImage(ctx, imageRef)
	if err != nil || !ok {
		t.Fatalf("HasImage = %v err=%v", ok, err)
	}
	t.Cleanup(func() {
		cx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = b.c.do(cx, "DELETE", "/images/"+urlQueryEscape(imageRef)+"?force=true", nil, nil)
	})

	// restore into a fresh sandbox: state must survive
	name := fmt.Sprintf("sbx-restore-%d", time.Now().UnixNano()%1_000_000_000)
	ref2, err := b.Create(ctx, runtime.CreateRequest{
		Name: name, Image: imageRef, CPUMillicores: 500, MemoryMiB: 128,
	})
	if err != nil {
		t.Fatalf("create from snapshot: %v", err)
	}
	if err := b.Start(ctx, ref2); err != nil {
		t.Fatalf("start from snapshot: %v", err)
	}
	t.Cleanup(func() {
		cx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = b.Remove(cx, ref2)
	})
	got, err := b.GetFile(ctx, ref2, "/root/state.txt", 1<<20)
	if err != nil {
		t.Fatalf("snapshot state missing after restore: %v", err)
	}
	if string(got) != "state-before-snapshot" {
		t.Fatalf("snapshot state corrupted: %q", got)
	}
}

func TestOfflineRootfsImport(t *testing.T) {
	b := needDocker(t)
	ctx := context.Background()

	// harvest busybox from the cached alpine image (the only registry touch;
	// the import path itself never needs one)
	work := t.TempDir()
	cx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := b.c.do(cx, "POST", "/containers/create?name="+"sbx-harvest-"+fmt.Sprint(time.Now().UnixNano()%1e6),
		nil, strings.NewReader(`{"Image":"`+testBaseImage+`","Cmd":["true"]}`))
	if err != nil {
		t.Skipf("harvest container: %v", err)
	}
	var cr struct {
		ID string `json:"Id"`
	}
	json.Unmarshal(out, &cr)
	defer func() {
		cx2, c2 := context.WithTimeout(context.Background(), 30*time.Second)
		defer c2()
		_, _ = b.c.do(cx2, "DELETE", "/containers/"+urlQueryEscape(cr.ID)+"?force=true", nil, nil)
	}()

	// busybox is dynamically linked against musl: without the loader the
	// image builds fine but every exec dies with 127 — exactly the failure
	// that motivated harvesting /lib together with /bin/busybox.
	binStream, binCloser, err := b.c.raw(cx, "GET", "/containers/"+urlQueryEscape(cr.ID)+"/archive",
		map[string]string{"path": "/bin/busybox"}, nil, nil)
	if err != nil {
		t.Skipf("harvest busybox: %v", err)
	}
	busy, err := extractSingleFile(binStream.Body)
	binCloser()
	if err != nil {
		t.Skipf("extract busybox: %v", err)
	}
	libStream, libCloser, err := b.c.raw(cx, "GET", "/containers/"+urlQueryEscape(cr.ID)+"/archive",
		map[string]string{"path": "/lib"}, nil, nil)
	if err != nil {
		t.Skipf("harvest /lib: %v", err)
	}
	libRoot := filepath.Join(work, "libdump")
	if err := extractTarInto(libStream.Body, libRoot); err != nil {
		t.Skipf("extract /lib: %v", err)
	}
	libCloser()

	// build a rootfs with merged-/usr layout: binaries live in usr/bin and
	// /bin must be a symlink to usr/bin — exactly the layout that broke twice
	rootfs := filepath.Join(work, "rootfs")
	for _, d := range []string{"usr/bin", "usr/lib", "etc", "tmp", "root", "proc", "dev", "var/tmp"} {
		if err := os.MkdirAll(filepath.Join(rootfs, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rootfs, "usr/bin/busybox"), busy, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(libRoot, "lib"))
	if err != nil {
		t.Skipf("no lib members harvested: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue // e.g. /lib/apk — no loader bits live in directories
		}
		src := filepath.Join(libRoot, "lib", e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(src)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(rootfs, "usr/lib", e.Name())); err != nil {
				t.Fatal(err)
			}
			continue
		}
		blob, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootfs, "usr/lib", e.Name()), blob, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, link := range []string{"sh", "echo", "sleep", "cat", "ls", "rm", "dd"} {
		if err := os.Symlink("busybox", filepath.Join(rootfs, "usr/bin", link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("usr/bin", filepath.Join(rootfs, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("usr/lib", filepath.Join(rootfs, "lib")); err != nil {
		t.Fatal(err)
	}

	tag := fmt.Sprintf("sandboxd-test-rootfs:%d", time.Now().UnixNano()%1_000_000_000)
	imageRef, size, err := b.ImportRootfs(ctx, rootfs, tag)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if size <= 0 {
		t.Fatalf("imported image size = %d", size)
	}
	t.Cleanup(func() {
		cx2, c2 := context.WithTimeout(context.Background(), 30*time.Second)
		defer c2()
		_, _ = b.c.do(cx2, "DELETE", "/images/"+urlQueryEscape(imageRef)+"?force=true", nil, nil)
	})

	name := fmt.Sprintf("sbx-rootfs-%d", time.Now().UnixNano()%1_000_000_000)
	ref, err := b.Create(ctx, runtime.CreateRequest{
		Name: name, Image: imageRef, CPUMillicores: 500, MemoryMiB: 128,
	})
	if err != nil {
		t.Fatalf("create from imported rootfs: %v", err)
	}
	if err := b.Start(ctx, ref); err != nil {
		t.Fatalf("start from imported rootfs: %v", err)
	}
	t.Cleanup(func() {
		cx2, c2 := context.WithTimeout(context.Background(), 30*time.Second)
		defer c2()
		_ = b.Remove(cx2, ref)
	})
	res, err := b.Exec(ctx, ref, runtime.ExecRequest{Argv: []string{"/bin/echo", "offline-ok"}, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.TrimSpace(string(res.Stdout)) != "offline-ok" {
		t.Fatalf("exec from imported rootfs: out=%q err=%q", res.Stdout, res.Stderr)
	}
}

func TestOrphansFindsLabeledContainers(t *testing.T) {
	b := needDocker(t)
	ref := mkSandbox(t, b, 128)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	orphans, err := b.Orphans(ctx)
	if err != nil {
		t.Fatalf("orphans: %v", err)
	}
	found := false
	for _, o := range orphans {
		if o.Ref == ref {
			found = true
			if o.Labels["product"] != "sandboxd" {
				t.Fatalf("product label missing: %+v", o.Labels)
			}
		}
	}
	if !found {
		t.Fatalf("labeled sandbox %s not in orphan list (%d entries)", ref, len(orphans))
	}
}

// extractSingleFile pulls the one file out of a docker archive GET response.
func extractSingleFile(r io.Reader) ([]byte, error) {
	tr := tar.NewReader(r)
	hdr, err := tr.Next()
	if err != nil {
		return nil, err
	}
	if hdr.Size <= 0 {
		return nil, fmt.Errorf("empty archive member")
	}
	out := make([]byte, hdr.Size)
	if _, err := io.ReadFull(tr, out); err != nil {
		return nil, err
	}
	return out, nil
}

// extractTarInto unpacks a docker archive response into dstDir.
func extractTarInto(r io.Reader, dstDir string) error {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := hdr.Name
		name = strings.TrimPrefix(name, "./")
		if name == "" || strings.Contains(name, "..") {
			continue
		}
		dst := filepath.Join(dstDir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			_ = os.Remove(dst)
			if err := os.Symlink(hdr.Linkname, dst); err != nil {
				return err
			}
		}
	}
}

func TestCopyDirGraftsTree(t *testing.T) {
	b := needDocker(t)
	ctx := context.Background()
	refA := mkSandbox(t, b, 128)
	refB := mkSandbox(t, b, 128)

	payload := []byte("grafted-toolkit-content")
	if err := b.PutFile(ctx, refA, runtime.PutFileRequest{Path: "/opt/toolkit/bin/run.sh", Content: payload, Mode: 0o755}); err != nil {
		t.Fatalf("seed source tree: %v", err)
	}
	if err := b.CopyDir(ctx, refA, "/opt/toolkit", refB, "/opt/toolkit"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	got, err := b.GetFile(ctx, refB, "/opt/toolkit/bin/run.sh", 1<<20)
	if err != nil {
		t.Fatalf("grafted file missing: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("grafted content mismatch: %q", got)
	}
}
