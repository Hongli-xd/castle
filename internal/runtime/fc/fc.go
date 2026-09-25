// Package fc is the Firecracker microVM backend skeleton.
//
// It exists so the Backend interface evolution is compile-checked against a
// second implementation, and so a Linux/KVM host can slot the real driver
// in behind the same contract. Every operation that would need to drive VMM
// state returns apperr.Unimplemented with the exact missing prerequisite —
// the daemon then refuses the create instead of pretending the stricter
// isolation exists. See docs/DESIGN.md §3.5 for the honesty contract.
//
// Roadmap notes (keep with the code):
//   - snapshot shape mirrors E2B: pre-booted VM state + memory diff + CoW
//     rootfs overlay in object storage; create == restore, same path for
//     templates, resumes, and forks;
//   - egress enforcement is nftables + SNI/Host inspection per sandbox,
//     which is why allowlist mode is promised here and refused in oci;
//   - disk quotas are enforced via ext4 project quotas on the VM image —
//     again backend-enforceable, unlike overlayfs on the shared host.
package fc

import (
	"context"
	"os"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/runtime"
)

// Available reports whether this node can drive microVMs.
func Available() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

type Backend struct{}

func New() *Backend { return &Backend{} }

func (b *Backend) Name() string { return "firecracker" }

func kvmRequired() error {
	return apperr.Unimpl("firecracker backend requires /dev/kvm; this host does not expose it — deploy on a Linux host with nested virtualization or bare metal")
}

func (b *Backend) Ping(ctx context.Context) error {
	if !Available() {
		return kvmRequired()
	}
	return nil
}

func (b *Backend) Create(ctx context.Context, req runtime.CreateRequest) (string, error) {
	return "", kvmRequired()
}

func (b *Backend) Start(ctx context.Context, ref string) error  { return kvmRequired() }
func (b *Backend) Pause(ctx context.Context, ref string) error  { return kvmRequired() }
func (b *Backend) Resume(ctx context.Context, ref string) error { return kvmRequired() }
func (b *Backend) Stop(ctx context.Context, ref string, force bool) error {
	return kvmRequired()
}
func (b *Backend) Remove(ctx context.Context, ref string) error          { return kvmRequired() }
func (b *Backend) Rename(ctx context.Context, ref, newName string) error { return kvmRequired() }
func (b *Backend) Inspect(ctx context.Context, ref string) (runtime.Status, error) {
	return runtime.Status{}, kvmRequired()
}

func (b *Backend) Exec(ctx context.Context, ref string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	return nil, kvmRequired()
}

func (b *Backend) PutFile(ctx context.Context, ref string, req runtime.PutFileRequest) error {
	return kvmRequired()
}

func (b *Backend) GetFile(ctx context.Context, ref, path string, maxSize int64) ([]byte, error) {
	return nil, kvmRequired()
}

func (b *Backend) ListDir(ctx context.Context, ref, path string) ([]runtime.FileInfo, error) {
	return nil, kvmRequired()
}

func (b *Backend) DeletePath(ctx context.Context, ref, path string) error { return kvmRequired() }

func (b *Backend) Commit(ctx context.Context, ref, tag string) (string, int64, error) {
	return "", 0, kvmRequired()
}

func (b *Backend) HasImage(ctx context.Context, imageRef string) (bool, error) {
	return false, kvmRequired()
}

func (b *Backend) RemoveImage(ctx context.Context, imageRef string) error { return kvmRequired() }

func (b *Backend) ImportRootfs(ctx context.Context, rootfsDir, tag string) (string, int64, error) {
	return "", 0, kvmRequired()
}

func (b *Backend) Orphans(ctx context.Context) ([]runtime.Orphan, error) {
	return nil, kvmRequired()
}
