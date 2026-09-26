package service

import (
	"context"
	"strings"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/idgen"
	"sandcastle.dev/sandcastle/internal/runtime"
)

// ComposeLayer is one toolkit layer grafted onto a template's base.
// Image is a backend image reference whose Path directory is grafted onto
// the same path of the base environment (same-path grafting keeps the
// semantics predictable: /opt/node20 in the layer is /opt/node20 in the
// composed environment).
type ComposeLayer struct {
	Image string `json:"image"`
	Path  string `json:"path"`
}

// ComposeTemplate materializes a layered template: base environment +
// toolkit layers grafted on top (DSec §5.1 environment composition,
// adapted to the container backend without dockerd modifications). The
// base image and each layer image remain independently versioned; only
// the composed result is a new image, and the composition chain is
// recorded for audit and rebuilds.
func (s *Service) ComposeTemplate(ctx context.Context, actorKeyID, name, base string, layers []ComposeLayer, spec domain.SandboxSpec, public bool) (*domain.Template, error) {
	if len(layers) == 0 {
		return nil, apperr.Invalid("compose requires at least one layer")
	}
	if len(layers) > 8 {
		return nil, apperr.Invalid("compose supports at most 8 layers")
	}
	baseImage := base
	if strings.HasPrefix(base, "tpl_") {
		tpl, ok := s.St.GetTemplate(base)
		if !ok {
			return nil, errMissing("base template not found")
		}
		baseImage = tpl.ImageRef
	}
	if ok, err := s.Backend.HasImage(ctx, baseImage); err != nil {
		return nil, err
	} else if !ok {
		return nil, errMissing("base image %q is not present on this node", baseImage)
	}

	grafts := make([]ComposeLayer, 0, len(layers))
	for _, l := range layers {
		if !strings.HasPrefix(l.Path, "/") || strings.Contains(l.Path, "..") || l.Path == "/" {
			return nil, apperr.Invalid("layer path must be an absolute in-sandbox directory: %q", l.Path)
		}
		if ok, err := s.Backend.HasImage(ctx, l.Image); err != nil {
			return nil, err
		} else if !ok {
			return nil, errMissing("layer image %q is not present on this node", l.Image)
		}
		grafts = append(grafts, l)
	}

	// scratch environment from the base; layers are grafted one by one and
	// their helper containers removed immediately. A daemon crash mid-
	// compose leaves a compose-labeled orphan the reconciler reaps by name.
	scratchRef, err := s.Backend.Create(ctx, composeCreateRequest(baseImage))
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Backend.Remove(context.Background(), scratchRef) }()
	if err := s.Backend.Start(ctx, scratchRef); err != nil {
		return nil, err
	}
	applied := make([]string, 0, len(grafts))
	for _, l := range grafts {
		helperRef, err := s.Backend.Create(ctx, composeCreateRequest(l.Image))
		if err != nil {
			return nil, err
		}
		if err := s.Backend.Start(ctx, helperRef); err != nil {
			_ = s.Backend.Remove(ctx, helperRef)
			return nil, err
		}
		err = s.Backend.CopyDir(ctx, helperRef, l.Path, scratchRef, l.Path)
		_ = s.Backend.Remove(ctx, helperRef)
		if err != nil {
			return nil, err
		}
		applied = append(applied, l.Image+"@"+l.Path)
	}

	tag := strings.ToLower("sandcastle-tpl-" + strings.ReplaceAll(name, " ", "-"))
	imageRef, _, err := s.Backend.Commit(ctx, scratchRef, tag)
	if err != nil {
		return nil, err
	}

	tpl := domain.NewTemplate("", name)
	tpl.ImageRef = imageRef
	tpl.DefaultSpec = spec
	tpl.Public = public
	tpl.Composition = append([]string{baseImage}, applied...)
	if err := tpl.Validate(s.Cfg.Limits); err != nil {
		_ = s.Backend.RemoveImage(ctx, imageRef)
		return nil, err
	}
	if err := s.St.PutTemplate(tpl); err != nil {
		_ = s.Backend.RemoveImage(ctx, imageRef)
		return nil, err
	}
	_ = s.Audit.Record("", "", actorKeyID, domain.ActTemplateCreate, map[string]string{
		"template": tpl.ID, "source": "compose", "layers": itoa(len(applied)),
	})
	return tpl, nil
}

func composeCreateRequest(image string) runtime.CreateRequest {
	return runtime.CreateRequest{
		Name:          "sandcastle-compose-" + strings.ToLower(idgen.New("c")[4:12]),
		Image:         image,
		CPUMillicores: 500,
		MemoryMiB:     512,
		Labels:        map[string]string{"compose": "1"},
	}
}
