package service

import (
	"context"
	"strings"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
)

// CreateSnapshot freezes a sandbox's writable filesystem into a restorable
// image. Quota is checked against the real committed size; a rejected
// snapshot deletes its image — nothing lingers that the caller was told no.
func (s *Service) CreateSnapshot(ctx context.Context, tenantID, keyID, sandboxID, name string) (*domain.Snapshot, error) {
	sb, err := s.loadOwned(tenantID, sandboxID)
	if err != nil {
		return nil, err
	}
	switch sb.State {
	case domain.StateRunning, domain.StatePaused:
	default:
		return nil, apperr.ConflictErr("snapshots require a running or paused sandbox, current state is %s", sb.State)
	}
	if sb.Backend.Ref == "" {
		return nil, apperr.ConflictErr("sandbox has no backend object to commit yet")
	}
	tenant, ok := s.St.GetTenant(tenantID)
	if !ok {
		return nil, errMissing("tenant not found")
	}

	sn := domain.NewSnapshot(tenantID, sb.ID, name)
	if err := sn.Validate(); err != nil {
		return nil, err
	}
	// name uniqueness within tenant
	for _, existing := range s.St.ListSnapshots(tenantID) {
		if existing.Name == name {
			return nil, apperr.ConflictErr("snapshot named %q already exists", name)
		}
	}

	// image repository names must be lowercase; sandbox IDs are uppercase
	tag := strings.ToLower("sandboxd-snap-" + sn.ID)
	imageRef, size, err := s.Backend.Commit(ctx, sb.Backend.Ref, tag)
	if err != nil {
		return nil, err
	}
	if err := s.Quotas.CheckSnapshot(tenant, size); err != nil {
		_ = s.Backend.RemoveImage(ctx, imageRef)
		return nil, err
	}
	sn.ImageRef = imageRef
	sn.SizeBytes = size
	sn.Spec = sb.Spec
	if err := s.St.PutSnapshot(sn); err != nil {
		_ = s.Backend.RemoveImage(ctx, imageRef)
		return nil, err
	}
	_ = s.Audit.Record(tenantID, sb.ID, keyID, domain.ActSnapshotCreate, map[string]string{"snapshot": sn.ID, "bytes": itoa(int(size))})
	s.Reg.Counter("sandboxd_snapshots_total", "snapshots created", nil, 1)
	return sn, nil
}

func (s *Service) GetSnapshot(tenantID, snapshotID string) (*domain.Snapshot, error) {
	if err := idgenCheck("snp", snapshotID); err != nil {
		return nil, err
	}
	sn, ok := s.St.GetSnapshot(snapshotID)
	if !ok {
		return nil, errMissing("snapshot not found")
	}
	if sn.TenantID != tenantID {
		return nil, errForbidden()
	}
	return sn, nil
}

func (s *Service) ListSnapshots(tenantID string) []*domain.Snapshot {
	return s.St.ListSnapshots(tenantID)
}

func (s *Service) DeleteSnapshot(ctx context.Context, tenantID, keyID, snapshotID string) error {
	sn, err := s.GetSnapshot(tenantID, snapshotID)
	if err != nil {
		return err
	}
	if err := s.St.DeleteSnapshot(sn.ID); err != nil {
		return err
	}
	if sn.ImageRef != "" {
		// image removal is best-effort: the record is gone either way, and
		// the node GC reaps untagged images
		_ = s.Backend.RemoveImage(ctx, sn.ImageRef)
	}
	_ = s.Audit.Record(tenantID, "", keyID, domain.ActSnapshotDelete, map[string]string{"snapshot": sn.ID})
	return nil
}

// ---- templates ------------------------------------------------------------

func (s *Service) CreateTemplate(ctx context.Context, tenantID, keyID, name, imageRef string, spec domain.SandboxSpec, public bool) (*domain.Template, error) {
	tpl := domain.NewTemplate(tenantID, name)
	tpl.ImageRef = imageRef
	tpl.DefaultSpec = spec
	tpl.Public = public
	if err := tpl.Validate(s.Cfg.Limits); err != nil {
		return nil, err
	}
	if ok, err := s.Backend.HasImage(ctx, imageRef); err != nil {
		return nil, err
	} else if !ok {
		return nil, errMissing("image %q is not present on this node", imageRef)
	}
	if err := s.St.PutTemplate(tpl); err != nil {
		return nil, err
	}
	_ = s.Audit.Record(tenantID, "", keyID, domain.ActTemplateCreate, map[string]string{"template": tpl.ID, "image": imageRef})
	return tpl, nil
}

func (s *Service) ListTemplates(tenantID string) []*domain.Template {
	return s.St.ListTemplates(tenantID)
}

func (s *Service) DeleteTemplate(ctx context.Context, tenantID, keyID, templateID string) error {
	if err := idgenCheck("tpl", templateID); err != nil {
		return err
	}
	tpl, ok := s.St.GetTemplate(templateID)
	if !ok {
		return errMissing("template not found")
	}
	if tpl.TenantID != tenantID {
		return errForbidden()
	}
	if err := s.St.DeleteTemplate(tpl.ID); err != nil {
		return err
	}
	_ = s.Audit.Record(tenantID, "", keyID, domain.ActTemplateDelete, map[string]string{"template": tpl.ID})
	return nil
}

// ImportRootfsTemplate imports a local rootfs directory as a backend image
// and registers it as a template. rootfs_dir must live on the daemon's host.
func (s *Service) ImportRootfsTemplate(ctx context.Context, actorKeyID, name, rootfsDir string, public bool, spec domain.SandboxSpec) (*domain.Template, error) {
	tag := "sandboxd-tpl-" + strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	imageRef, _, err := s.Backend.ImportRootfs(ctx, rootfsDir, tag)
	if err != nil {
		return nil, err
	}
	tpl := domain.NewTemplate("", name)
	tpl.ImageRef = imageRef
	tpl.DefaultSpec = spec
	tpl.Public = public
	if err := tpl.Validate(s.Cfg.Limits); err != nil {
		_ = s.Backend.RemoveImage(ctx, imageRef)
		return nil, err
	}
	if err := s.St.PutTemplate(tpl); err != nil {
		_ = s.Backend.RemoveImage(ctx, imageRef)
		return nil, err
	}
	_ = s.Audit.Record("", "", actorKeyID, domain.ActTemplateCreate, map[string]string{"template": tpl.ID, "source": "rootfs"})
	return tpl, nil
}
