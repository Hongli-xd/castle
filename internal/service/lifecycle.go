package service

import (
	"context"
	"strconv"
	"sync"
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

// requireRunning enforces the running precondition, with DSec §6.3
// transparent-resume semantics: a request to a paused sandbox resumes it
// first (config auto_resume, default on) instead of bouncing with 409.
// A frozen workload is an implementation detail of pause, not something
// callers should have to manage.
func (s *Service) requireRunning(ctx context.Context, tenantID, keyID string, sb *domain.Sandbox) (*domain.Sandbox, error) {
	if sb.State == domain.StateRunning {
		return sb, nil
	}
	if sb.State == domain.StatePaused && s.Cfg.AutoResume {
		if _, err := s.ResumeSandbox(ctx, tenantID, keyID, sb.ID); err != nil {
			return nil, err
		}
		return s.GetSandbox(tenantID, sb.ID)
	}
	return nil, apperr.ConflictErr("sandbox must be running, current state is %s", sb.State)
}

// touchActivity records user activity for idle-timeout reclamation.
// Persisted at most once per sandbox per minute: fresh enough for the
// sweeper, without an fsync on every tool call.
func (s *Service) touchActivity(tenantID, sandboxID string) {
	now := time.Now().UTC()
	s.activityMu.Lock()
	if last, seen := s.activitySeen[sandboxID]; seen && now.Sub(last) < time.Minute {
		s.activityMu.Unlock()
		return
	}
	s.activitySeen[sandboxID] = now
	s.activityMu.Unlock()
	_, _ = s.St.UpdateSandbox(sandboxID, -1, func(x *domain.Sandbox) error {
		x.LastActivityAt = now
		return nil
	})
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
	s.Reg.Counter("sandboxd_transitions_total", "sandbox state transitions", []string{"action", action}, 1)
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
		func() error {
			opts := runtime.PauseOptions{}
			if s.Cfg.PauseReclaim {
				opts.MemoryBytes = int64(sb.Spec.MemoryMiB) << 20
			}
			return s.Backend.Pause(ctx, sb.Backend.Ref, opts)
		},
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
		func() error {
			opts := runtime.PauseOptions{}
			if s.Cfg.PauseReclaim {
				opts.MemoryBytes = int64(sb.Spec.MemoryMiB) << 20
			}
			return s.Backend.Resume(ctx, sb.Backend.Ref, opts)
		},
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
	// The record deletion is deliberately CAS-free (-1). Delete is a terminal,
	// idempotent operation: once force=true clears the active-state guard, the
	// client's goal is "this sandbox is gone", and that goal does not change if
	// a bookkeeping write (touchActivity, the reconciler, the sweeper — all of
	// which bump Version even with baseVersion=-1) landed between loadOwned and
	// here. Guarding the delete with sb.Version turned that harmless bump into a
	// 409 that arrived *after* the container had already been removed — leaving
	// the record stranded and the client with a spurious conflict. A concurrent
	// delete's NotFound is equally the client's goal met, so it is success too.
	if err := s.St.DeleteSandbox(sb.ID, -1); err != nil && apperr.From(err).Code != apperr.NotFound {
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

func (s *Service) Exec(ctx context.Context, tenantID, keyID, sandboxID string, req runtime.ExecRequest, idemKey string) (*runtime.ExecResult, error) {
	sb, err := s.loadOwned(tenantID, sandboxID)
	if err != nil {
		return nil, err
	}
	if sb, err = s.requireRunning(ctx, tenantID, keyID, sb); err != nil {
		return nil, err
	}
	s.touchActivity(tenantID, sb.ID)
	if req.Timeout > s.Cfg.MaxExecTimeout {
		return nil, apperr.Invalid("exec timeout exceeds the platform maximum of %s", s.Cfg.MaxExecTimeout)
	}
	if req.Timeout <= 0 {
		req.Timeout = 60 * time.Second
	}
	// max_output is a CEILING, not a preference. The control plane buffers the
	// whole stdout+stderr in its own heap before trimming, so without this
	// check one request could ask for 2 GB and make the daemon allocate
	// hundreds of MiB of shared memory (poc/probe_live.sh, PoC-4). A negative
	// value is rejected rather than treated as "default": the JSON field is
	// int, so a client error would otherwise silently become a large unsigned
	// cast downstream.
	switch {
	case req.MaxOutput < 0:
		return nil, apperr.Invalid("exec max_output must not be negative")
	case int64(req.MaxOutput) > s.Cfg.MaxExecOutput:
		return nil, apperr.Invalid("exec max_output exceeds the platform maximum of %d bytes (0 selects the %s backend default of %d bytes)",
			s.Cfg.MaxExecOutput, s.Backend.Name(), runtime.DefaultOutputCap)
	}

	// The actual execution — audit, metrics, metering, backend round trip —
	// runs only when the command really runs. A replay must not emit a second
	// sandbox.exec, count as a second exec, or bill a second command.
	//
	// The audit event's WAL fsync is the only sandboxd-side cost on this path
	// that is not already amortised (measured ~4–5 ms on an SSD; see
	// CHANGELOG v1.1.8). It is issued on its own goroutine so it overlaps the
	// backend's Docker round-trips instead of serialising ahead of them, and
	// auditDone.Wait() before returning keeps the durability contract intact:
	// a confirmed exec (the response reaching the client) still implies its
	// sandbox.exec event is fsynced — THREAT-MODEL "crash never drops an
	// acknowledged event" is unchanged. The only shift is inside the request:
	// the event used to be durable *before* the command started, now it is
	// durable *before the response leaves*. A crash in that window (after the
	// command started, before its audit landed) can drop the audit record for
	// an exec whose response never reached the client.
	exec := func() (*runtime.ExecResult, error) {
		var auditDone sync.WaitGroup
		auditDone.Add(1)
		go func() {
			defer auditDone.Done()
			_ = s.Audit.Record(tenantID, sb.ID, keyID, domain.ActSandboxExec, map[string]string{"argv0": sanitize(first(req.Argv))})
		}()
		s.Reg.Counter("sandboxd_execs_total", "sandbox execs", nil, 1)
		start := time.Now()
		res, err := s.Backend.Exec(ctx, sb.Backend.Ref, req)
		s.meter.RecordExec(tenantID, time.Since(start))
		auditDone.Wait()
		return res, err
	}

	if idemKey == "" {
		return exec()
	}
	if err := validateIdempotencyKey(idemKey); err != nil {
		return nil, err
	}
	key := tenantID + "\x00" + sandboxID + "\x00" + idemKey
	res, replayed, err := s.idem.run(ctx, key, execFingerprint(req), exec)
	if err == nil && replayed {
		res.Replayed = true
		s.Reg.Counter("sandboxd_execs_replayed_total", "exec responses served from the idempotency cache", nil, 1)
	}
	return res, err
}

func (s *Service) PutFile(ctx context.Context, tenantID, keyID, sandboxID, path string, content []byte, mode uint32) error {
	sb, err := s.loadOwned(tenantID, sandboxID)
	if err != nil {
		return err
	}
	if sb, err = s.requireRunning(ctx, tenantID, keyID, sb); err != nil {
		return err
	}
	s.touchActivity(tenantID, sb.ID)
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
	if sb, err = s.requireRunning(ctx, tenantID, keyID, sb); err != nil {
		return nil, err
	}
	s.touchActivity(tenantID, sb.ID)
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
	if sb, err = s.requireRunning(ctx, tenantID, keyID, sb); err != nil {
		return nil, err
	}
	s.touchActivity(tenantID, sb.ID)
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
	if sb, err = s.requireRunning(ctx, tenantID, keyID, sb); err != nil {
		return err
	}
	s.touchActivity(tenantID, sb.ID)
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
