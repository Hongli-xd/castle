package service

import (
	"context"
	"strconv"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/runtime"
)

func itoa(n int) string { return strconv.Itoa(n) }

// loadOwned fetches a sandbox and enforces tenant scoping — the IDOR
// boundary. Admin keys act within their own (platform) tenant for sandbox
// resources; cross-tenant access is a 404-shaped 403 to avoid existence
// leaks... it is a 403, but the message does not confirm existence.
func (s *Service) loadOwned(tenantID, sandboxID string) (*domain.Sandbox, error) {
	if err := idgenCheck("sbx", sandboxID); err != nil {
		return nil, err
	}
	sb, ok := s.St.GetSandbox(sandboxID)
	if !ok {
		return nil, errMissing("sandbox not found")
	}
	if sb.TenantID != tenantID {
		return nil, errForbidden()
	}
	return sb, nil
}

// transition drives one state transition with the backend call in between:
// record → pausing → backend.Pause → paused. Intermediate failures revert
// the record when the backend says the sandbox is still healthy, else leave
// a transitional state for the reconciler.
func (s *Service) transition(ctx context.Context, sb *domain.Sandbox, mid, done domain.SandboxState, fn func() error, action, actor string) (*domain.Sandbox, error) {
	updated, err := s.St.UpdateSandbox(sb.ID, sb.Version, func(x *domain.Sandbox) error {
		return x.Transition(mid)
	})
	if err != nil {
		return nil, err
	}
	if err := fn(); err != nil {
		// revert only when the backend confirms health; otherwise the
		// reconciler owns the truth
		st, ierr := s.Backend.Inspect(ctx, sb.Backend.Ref)
		switch {
		case ierr != nil:
			// backend unreachable: leave transitional state, reconciler fixes
		case st.Running && mid == domain.StatePausing:
			_, _ = s.St.UpdateSandbox(sb.ID, -1, func(x *domain.Sandbox) error { return x.Transition(domain.StateRunning) })
		case !st.Running && mid == domain.StateResuming:
			_, _ = s.St.UpdateSandbox(sb.ID, -1, func(x *domain.Sandbox) error { return x.Transition(domain.StatePaused) })
		}
		return nil, err
	}
	updated, err = s.St.UpdateSandbox(sb.ID, -1, func(x *domain.Sandbox) error {
		return x.Transition(done)
	})
	if err != nil {
		return nil, err
	}
	_ = s.Audit.Record(sb.TenantID, sb.ID, actor, action, nil)
	s.Reg.Counter("sandboxd_transitions_total", "sandbox state transitions", []string{"action"}, 1)
	return updated, nil
}

func (s *Service) PauseSandbox(ctx context.Context, tenantID, keyID, sandboxID string) (*domain.Sandbox, error) {
	sb, err := s.loadOwned(tenantID, sandboxID)
	if err != nil {
		return nil, err
	}
	if sb.State != domain.StateRunning {
		return nil, apperr.ConflictErr("pause requires a running sandbox, current state is %s", sb.State)
	}
	return s.transition(ctx, sb, domain.StatePausing, domain.StatePaused,
		func() error { return s.Backend.Pause(ctx, sb.Backend.Ref) },
		domain.ActSandboxPause, keyID)
}

func (s *Service) ResumeSandbox(ctx context.Context, tenantID, keyID, sandboxID string) (*domain.Sandbox, error) {
	sb, err := s.loadOwned(tenantID, sandboxID)
	if err != nil {
		return nil, err
	}
	if sb.State != domain.StatePaused {
		return nil, apperr.ConflictErr("resume requires a paused sandbox, current state is %s", sb.State)
	}
	return s.transition(ctx, sb, domain.StateResuming, domain.StateRunning,
		func() error { return s.Backend.Resume(ctx, sb.Backend.Ref) },
		domain.ActSandboxResume, keyID)
}

// StopSandbox gracefully stops; force escalates to an immediate kill.
func (s *Service) StopSandbox(ctx context.Context, tenantID, keyID, sandboxID string, force bool) (*domain.Sandbox, error) {
	sb, err := s.loadOwned(tenantID, sandboxID)
	if err != nil {
		return nil, err
	}
	switch sb.State {
	case domain.StateStopped:
		return sb, nil // idempotent
	case domain.StateRunning, domain.StatePaused:
	default:
		return nil, apperr.ConflictErr("cannot stop a sandbox in state %s", sb.State)
	}
	return s.transition(ctx, sb, domain.StateStopping, domain.StateStopped,
		func() error { return s.Backend.Stop(ctx, sb.Backend.Ref, force) },
		domain.ActSandboxStop, keyID)
}

// DeleteSandbox removes the record and the backend object. Active sandboxes
// require force=true (the client asked for an explicit destructive act).
func (s *Service) DeleteSandbox(ctx context.Context, tenantID, keyID, sandboxID string, force bool) error {
	sb, err := s.loadOwned(tenantID, sandboxID)
	if err != nil {
		return err
	}
	if !sb.State.Terminal() && !force {
		return apperr.ConflictErr("sandbox is %s — stop it first or pass force=true", sb.State)
	}
	if sb.Backend.Ref != "" {
		if err := s.Backend.Remove(ctx, sb.Backend.Ref); err != nil {
			return err
		}
	}
	if err := s.St.DeleteSandbox(sb.ID, sb.Version); err != nil {
		// record changed underneath (e.g. TTL fired concurrently): surface
		// a conflict, the sweeper will finish the job
		return err
	}
	_ = s.Audit.Record(tenantID, sb.ID, keyID, domain.ActSandboxDelete, map[string]string{"force": boolStr(force)})
	return nil
}

func (s *Service) ExtendSandbox(ctx context.Context, tenantID, keyID, sandboxID string, seconds int) (*domain.Sandbox, error) {
	sb, err := s.loadOwned(tenantID, sandboxID)
	if err != nil {
		return nil, err
	}
	if sb.State.Terminal() {
		return nil, apperr.ConflictErr("cannot extend a %s sandbox", sb.State)
	}
	if seconds <= 0 {
		seconds = sb.Spec.TimeoutSeconds
	}
	if err := sb.Extend(seconds, s.Cfg.Limits); err != nil {
		return nil, err
	}
	out, err := s.St.UpdateSandbox(sb.ID, sb.Version, func(x *domain.Sandbox) error {
		x.ExpiresAt = sb.ExpiresAt
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = s.Audit.Record(tenantID, sb.ID, keyID, domain.ActSandboxExtend, map[string]string{"seconds": itoa(seconds)})
	return out, nil
}

func (s *Service) GetSandbox(tenantID, sandboxID string) (*domain.Sandbox, error) {
	return s.loadOwned(tenantID, sandboxID)
}

func (s *Service) ListSandboxes(tenantID string, state *domain.SandboxState) []*domain.Sandbox {
	return s.St.ListSandboxes(tenantID, state)
}

// ---- exec & files ---------------------------------------------------------

func (s *Service) Exec(ctx context.Context, tenantID, keyID, sandboxID string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	sb, err := s.loadOwned(tenantID, sandboxID)
	if err != nil {
		return nil, err
	}
	if sb.State != domain.StateRunning {
		return nil, apperr.ConflictErr("exec requires a running sandbox, current state is %s", sb.State)
	}
	if req.Timeout > s.Cfg.MaxExecTimeout {
		return nil, apperr.Invalid("exec timeout exceeds the platform maximum of %s", s.Cfg.MaxExecTimeout)
	}
	if req.Timeout <= 0 {
		req.Timeout = 60 * time.Second
	}
	_ = s.Audit.Record(tenantID, sb.ID, keyID, domain.ActSandboxExec, map[string]string{"argv0": sanitize(first(req.Argv))})
	s.Reg.Counter("sandboxd_execs_total", "sandbox execs", nil, 1)
	return s.Backend.Exec(ctx, sb.Backend.Ref, req)
}

func (s *Service) PutFile(ctx context.Context, tenantID, keyID, sandboxID, path string, content []byte, mode uint32) error {
	sb, err := s.loadOwned(tenantID, sandboxID)
	if err != nil {
		return err
	}
	if sb.State != domain.StateRunning {
		return apperr.ConflictErr("file operations require a running sandbox, current state is %s", sb.State)
	}
	if err := validatePath(path); err != nil {
		return err
	}
	if int64(len(content)) > s.Cfg.MaxFileBytes {
		return apperr.Quota("file exceeds the %d byte upload cap", s.Cfg.MaxFileBytes)
	}
	if err := s.Backend.PutFile(ctx, sb.Backend.Ref, runtime.PutFileRequest{Path: path, Content: content, Mode: mode}); err != nil {
		return err
	}
	_ = s.Audit.Record(tenantID, sb.ID, keyID, domain.ActFilePut, map[string]string{"path": sanitize(path), "bytes": itoa(len(content))})
	return nil
}

func (s *Service) GetFile(ctx context.Context, tenantID, keyID, sandboxID, path string) ([]byte, error) {
	sb, err := s.loadOwned(tenantID, sandboxID)
	if err != nil {
		return nil, err
	}
	if sb.State != domain.StateRunning {
		return nil, apperr.ConflictErr("file operations require a running sandbox, current state is %s", sb.State)
	}
	if err := validatePath(path); err != nil {
		return nil, err
	}
	data, err := s.Backend.GetFile(ctx, sb.Backend.Ref, path, s.Cfg.MaxFileBytes)
	if err == nil {
		_ = s.Audit.Record(tenantID, sb.ID, keyID, domain.ActFileGet, map[string]string{"path": sanitize(path), "bytes": itoa(len(data))})
	}
	return data, err
}

func (s *Service) ListFiles(ctx context.Context, tenantID, keyID, sandboxID, path string) ([]runtime.FileInfo, error) {
	sb, err := s.loadOwned(tenantID, sandboxID)
	if err != nil {
		return nil, err
	}
	if sb.State != domain.StateRunning {
		return nil, apperr.ConflictErr("file operations require a running sandbox, current state is %s", sb.State)
	}
	if err := validatePath(path); err != nil {
		return nil, err
	}
	return s.Backend.ListDir(ctx, sb.Backend.Ref, path)
}

func (s *Service) DeleteFile(ctx context.Context, tenantID, keyID, sandboxID, path string) error {
	sb, err := s.loadOwned(tenantID, sandboxID)
	if err != nil {
		return err
	}
	if sb.State != domain.StateRunning {
		return apperr.ConflictErr("file operations require a running sandbox, current state is %s", sb.State)
	}
	if err := validatePath(path); err != nil {
		return err
	}
	if err := s.Backend.DeletePath(ctx, sb.Backend.Ref, path); err != nil {
		return err
	}
	_ = s.Audit.Record(tenantID, sb.ID, keyID, domain.ActFileDelete, map[string]string{"path": sanitize(path)})
	return nil
}

func first(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return argv[0]
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
