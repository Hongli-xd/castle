package service

import (
	"fmt"
	"regexp"
	"strings"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/idgen"
)

func errSuspended() error {
	return apperr.ForbiddenErr("tenant is suspended")
}

func errForbidden() error {
	return apperr.ForbiddenErr("resource belongs to another tenant")
}

func errMissing(format string, args ...any) error {
	return apperr.New(apperr.NotFound, fmt.Sprintf(format, args...))
}

func errCreateTimeout(d interface{ String() string }) error {
	return apperr.New(apperr.Internal, fmt.Sprintf("sandbox did not reach running within %s; it may still appear shortly", d))
}

func errFromCtx(ctx interface{ Err() error }) error {
	return apperr.From(ctx.Err())
}

// SandboxFailedError distinguishes "create finished but failed" from
// transport errors so clients can render it accurately (HTTP 409 + the
// sandbox record for post-mortem).
type SandboxFailedError struct{ reason string }

func (e *SandboxFailedError) Error() string {
	return "sandbox provisioning failed: " + e.reason
}

func (e *SandboxFailedError) Reason() string { return e.reason }

// idgenCheck validates that id carries the expected kind prefix.
func idgenCheck(prefix, id string) error {
	return idgen.Parse(prefix, id)
}

var metadataKeyRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

func validateMetadata(md map[string]string, l domain.Limits) error {
	if len(md) > l.MaxMetadataKeys {
		return apperr.Invalid("metadata supports at most %d keys", l.MaxMetadataKeys)
	}
	total := 0
	for k, v := range md {
		if !metadataKeyRe.MatchString(k) {
			return apperr.Invalid("invalid metadata key %q", k)
		}
		total += len(k) + len(v)
	}
	if total > l.MaxMetadataBytes*1024 {
		return apperr.Invalid("metadata exceeds %d KiB", l.MaxMetadataBytes)
	}
	return nil
}

func diskQuotaRefused() error {
	return apperr.New(apperr.Unimplemented,
		"this node cannot enforce per-sandbox disk quotas on its filesystem; "+
			"create without disk_mib, or have the operator start the daemon with "+
			"enforce_disk_quota=false to accept unenforced limits explicitly")
}

// validatePath guards every file operation: absolute, no traversal, no
// control characters, bounded length. It is the service-level line of
// defense; backends re-check.
func validatePath(p string) error {
	if p == "" {
		return apperr.Invalid("path is required")
	}
	if !strings.HasPrefix(p, "/") {
		return apperr.Invalid("path must be absolute")
	}
	if len(p) > 4096 {
		return apperr.Invalid("path exceeds 4096 bytes")
	}
	if strings.Contains(p, "\x00") {
		return apperr.Invalid("path contains NUL")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return apperr.Invalid("path traversal ('..') is rejected")
		}
	}
	return nil
}

// validateCleanupPaths enforces the pack-cleanup contract: absolute, no
// traversal, bounded count, and never a system root — deleting /usr inside
// a packed environment is a footgun the platform must refuse.
func validateCleanupPaths(paths []string) error {
	if len(paths) > 16 {
		return apperr.Invalid("pack.cleanup supports at most 16 paths")
	}
	for _, p := range paths {
		if err := validatePath(p); err != nil {
			return err
		}
		if p == "/" {
			return apperr.Invalid("pack.cleanup refuses to delete /")
		}
		for _, forbidden := range []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/root", "/sbin", "/sys", "/usr", "/var"} {
			if p == forbidden {
				return apperr.Invalid("pack.cleanup refuses system path %s", p)
			}
		}
	}
	return nil
}
