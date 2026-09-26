package service

import (
	"context"
	"strings"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/runtime"
)

// The reconciler is the crash-recovery brain. Its one rule: local records
// that describe in-flight operations are hypotheses; the backend is truth.
// Every non-terminal record is compared against the backend and converged;
// backend objects without records (crash residue) are reaped.

func (s *Service) reconcileLoop() {
	t := time.NewTicker(s.Cfg.ReconcileEvery)
	for {
		select {
		case <-s.stopCh:
			t.Stop()
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			s.Reconcile(ctx)
			cancel()
		}
	}
}

func (s *Service) sweepLoop() {
	t := time.NewTicker(s.Cfg.SweepEvery)
	for {
		select {
		case <-s.stopCh:
			t.Stop()
			return
		case <-t.C:
			s.sweepExpired()
		}
	}
}

func (s *Service) compactLoop() {
	t := time.NewTicker(s.Cfg.CompactEvery)
	for {
		select {
		case <-s.stopCh:
			t.Stop()
			return
		case <-t.C:
			if err := s.St.Compact(); err != nil {
				_ = s.Audit.Record("", "", "system", domain.ActReconcileFix, map[string]string{"note": "compaction failed: " + sanitize(err.Error())})
			}
		}
	}
}

// sweepExpired stops sandboxes past their hard TTL or their idle timeout
// (DSec §2.3 lifetime policy: both exist so abandoned or long-idle
// sandboxes cannot pin memory and quota forever — expiry is a governance
// event, not an error, and is audited as such).
func (s *Service) sweepExpired() {
	now := time.Now()
	type due struct {
		sb   *domain.Sandbox
		idle bool
	}
	var expired []due
	s.St.ForEachSandbox(func(sb *domain.Sandbox) bool {
		if sb.State.Terminal() {
			return true
		}
		switch {
		case sb.Expired(now):
			expired = append(expired, due{sb: sb})
		case sb.IdleExpired(now) && (sb.State == domain.StateRunning || sb.State == domain.StatePaused):
			expired = append(expired, due{sb: sb, idle: true})
		}
		return true
	})
	for _, d := range expired {
		sb := d.sb
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if sb.Backend.Ref != "" {
			_, _ = s.St.UpdateSandbox(sb.ID, -1, func(x *domain.Sandbox) error {
				if x.State == domain.StateRunning {
					return x.Transition(domain.StateStopping)
				}
				return nil
			})
			_ = s.Backend.Stop(ctx, sb.Backend.Ref, false)
		}
		_, err := s.St.UpdateSandbox(sb.ID, -1, func(x *domain.Sandbox) error {
			switch x.State {
			case domain.StateStopping, domain.StateRunning:
				return x.Transition(domain.StateStopped)
			}
			return nil
		})
		if err == nil {
			action := domain.ActSandboxExpire
			detail := map[string]string{"ttl_seconds": itoa(sb.Spec.TimeoutSeconds)}
			if d.idle {
				action = domain.ActSandboxIdleExpire
				detail = map[string]string{"idle_timeout_seconds": itoa(sb.Spec.IdleTimeoutSeconds)}
			}
			_ = s.Audit.Record(sb.TenantID, sb.ID, "system", action, detail)
		}
		cancel()
	}
}

// Reconcile converges every non-terminal record with backend reality and
// reaps orphaned backend objects.
func (s *Service) Reconcile(ctx context.Context) { s.reconcile(ctx, false) }

// ReconcileAtBoot is the startup pass. After a process death nothing is
// "in flight" anymore: the usual provisioning grace period does not apply,
// so every unconverged record is repaired immediately.
func (s *Service) ReconcileAtBoot(ctx context.Context) { s.reconcile(ctx, true) }

func (s *Service) reconcile(ctx context.Context, atBoot bool) {
	fixed := 0
	reaped := 0

	var records []*domain.Sandbox
	s.St.ForEachSandbox(func(sb *domain.Sandbox) bool {
		records = append(records, sb)
		return true
	})
	knownRefs := map[string]bool{}
	for _, sb := range records {
		if sb.Backend.Ref != "" {
			knownRefs[sb.Backend.Ref] = true
		}
	}

	for _, sb := range records {
		if sb.State.Terminal() {
			continue
		}
		// provisioning never reached backend-create: nothing to inspect
		if sb.Backend.Ref == "" {
			// during periodic ticks a fresh provisioning record may still be
			// queued; after a reboot nothing is in flight, repair at once
			if !atBoot && sb.State == domain.StateProvisioning && time.Since(sb.CreatedAt) < 5*time.Minute {
				continue
			}
			s.failSandbox(sb.ID, "provisioning interrupted before backend object existed")
			fixed++
			continue
		}
		st, err := s.Backend.Inspect(ctx, sb.Backend.Ref)
		if err != nil {
			if apperr.From(err).Code == apperr.NotFound {
				s.failSandbox(sb.ID, "backend object disappeared (reconciled)")
				fixed++
			}
			continue // transient backend errors: try next tick
		}
		truth := backendToState(st)
		if truth == sb.State {
			continue
		}
		_, err = s.St.UpdateSandbox(sb.ID, -1, func(x *domain.Sandbox) error {
			x.State = truth // reconciler sets truth directly, state machine governs client paths
			if st.OOMKilled {
				x.Reason = "killed by memory limiter (reconciled)"
			} else if truth == domain.StateStopped {
				x.Reason = "stopped externally (reconciled)"
			}
			return nil
		})
		if err == nil {
			fixed++
			_ = s.Audit.Record(sb.TenantID, sb.ID, "system", domain.ActReconcileFix, map[string]string{
				"was": string(sb.State), "now": string(truth),
			})
		}
	}

	orphans, err := s.Backend.Orphans(ctx)
	if err != nil {
		return
	}
	for _, o := range orphans {
		if knownRefs[o.Ref] {
			continue
		}
		if isWarmName(o.Ref) {
			// pool inventory without a store record: live only if the
			// in-memory pool still tracks it. A crashed daemon's leftovers
			// are pristine (never exec'd) and safe to reap unconditionally.
			if !s.isLivePoolItem(o.Ref) {
				_ = s.Backend.Remove(ctx, o.Ref)
				reaped++
			}
			continue
		}
		if isComposeName(o.Ref) {
			// template-composition scratch/helper containers have no store
			// record either; leftovers mean a daemon died mid-compose
			_ = s.Backend.Remove(ctx, o.Ref)
			reaped++
			continue
		}
		sid := o.Labels["sandbox.id"]
		if sid == "" {
			continue // ours by product label but not attributable; leave for ops
		}
		// a sandbox-labeled object with no record: crash residue from a
		// delete whose record removal succeeded first — reap it
		if _, ok := s.St.GetSandbox(sid); !ok {
			_ = s.Backend.Remove(ctx, o.Ref)
			reaped++
		}
	}
	if fixed > 0 || reaped > 0 {
		_ = s.Audit.Record("", "", "system", domain.ActReconcileFix, map[string]string{
			"fixed": itoa(fixed), "reaped": itoa(reaped),
		})
	}

}

func backendToState(st runtime.Status) domain.SandboxState {
	switch {
	case st.Paused:
		return domain.StatePaused
	case st.Running:
		return domain.StateRunning
	default:
		return domain.StateStopped
	}
}

// isWarmName matches pool-created backend objects. Names are the stable
// identity for pool items because labels cannot be updated on a container.
func isWarmName(ref string) bool {
	return len(ref) > 14 && ref[:14] == "sandboxd-warm-"
}

// isComposeName matches template-composition scratch containers.
func isComposeName(ref string) bool {
	return strings.HasPrefix(ref, "sandcastle-compose-")
}
