package service

import (
	"context"
	"strings"
	"time"

	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/idgen"
	"sandcastle.dev/sandcastle/internal/runtime"
)

// ---- warm pool ----------------------------------------------------------------
//
// The pool keeps N pristine *started* containers per public template.
// Pristine is the safety property: warm containers have never executed a
// workload, so handing one to any tenant leaks nothing. Adoption keeps the
// warm container's existing name and points the record at it — the record is
// the handoff point, so a crash before it leaves an orphan the reconciler
// reaps by age.

type poolItem struct {
	ref      string
	created  time.Time
	template string
}

// poolAdopt tries to hand out a warm container for the image the request
// resolves to. It mutates sb (state=running, backend ref set) on success.
//
// Adoption keeps the warm item's backend name — no rename, so there is no
// window where a backend object's identity is in flux. The record is the
// handoff point: before it, the item is pool inventory (reapable on daemon
// restart); after it, the store owns it.
func (s *Service) poolAdopt(sb *domain.Sandbox, imageRef string) (*domain.Sandbox, bool) {
	if !s.Cfg.PoolEnabled || s.Cfg.PoolSize <= 0 {
		return sb, false
	}
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	items := s.pool[imageRef]
	if len(items) == 0 {
		s.Reg.Counter("sandboxd_pool_misses_total", "warm pool misses", nil, 1)
		return sb, false
	}
	item := items[0]
	s.pool[imageRef] = items[1:]

	sb.Backend = domain.SandboxBackendRef{Kind: s.Backend.Name(), Ref: item.ref, ImageRef: imageRef}
	sb.State = domain.StateRunning
	s.Reg.Counter("sandboxd_pool_hits_total", "warm pool hits", nil, 1)
	return sb, true
}

// isLivePoolItem reports whether ref is currently pool inventory. The
// reconciler consults it under the same lock to avoid reaping an item that
// is mid-adoption.
func (s *Service) isLivePoolItem(ref string) bool {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	for _, items := range s.pool {
		for _, it := range items {
			if it.ref == ref {
				return true
			}
		}
	}
	return false
}

func (s *Service) poolLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.poolFill()
		}
	}
}

// poolFill tops the pool up toward the configured size. Templates are
// discovered from the store (public platform templates only).
func (s *Service) poolFill() {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if s.pool == nil {
		s.pool = map[string][]poolItem{}
	}
	current := 0
	for _, items := range s.pool {
		current += len(items)
	}
	if current >= s.Cfg.PoolSize {
		s.Reg.Gauge("sandboxd_pool_size", "warm containers waiting", nil, float64(current))
		return
	}
	candidates := []*domain.Template{}
	for _, tpl := range s.St.ListTemplates("") {
		if tpl.Public {
			candidates = append(candidates, tpl)
		}
	}
	if len(candidates) == 0 {
		return
	}
	tpl := candidates[0]

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ref, err := s.Backend.Create(ctx, runtime.CreateRequest{
		Name:          "sandboxd-warm-" + strings.ToLower(idgen.New("w")[4:13]),
		Image:         tpl.ImageRef,
		CPUMillicores: specOr(tpl.DefaultSpec.CPUMillicores, s.Cfg.DefaultCPUMillicores),
		MemoryMiB:     specOr(tpl.DefaultSpec.MemoryMiB, s.Cfg.DefaultMemoryMiB),
		Labels:        map[string]string{"warm": "1", "template.id": tpl.ID},
	})
	if err != nil {
		return
	}
	if err := s.Backend.Start(ctx, ref); err != nil {
		_ = s.Backend.Remove(ctx, ref)
		return
	}
	s.pool[tpl.ImageRef] = append(s.pool[tpl.ImageRef], poolItem{ref: ref, created: time.Now(), template: tpl.ID})
	s.Reg.Gauge("sandboxd_pool_size", "warm containers waiting", nil, float64(current+1))
}

func specOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}
