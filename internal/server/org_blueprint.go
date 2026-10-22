// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"errors"

	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/internal/blueprint"
	dbpkg "github.com/k8shell-io/provisioner/internal/db"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CreateBlueprint stores a new org-scoped blueprint in the database. The
// blueprint's name, description and is_template flag are read from the
// submitted YAML document, which is validated exactly as ValidateBlueprint
// does before it is persisted; the blueprint manager's merged view is then
// refreshed so the new blueprint is immediately usable.
func (p *ProvisionerService) CreateBlueprint(_ context.Context,
	req *provisionerv1.CreateBlueprintRequest) (*provisionerv1.OrgBlueprint, error) {
	if req.GetOrg() == "" {
		return nil, status.Error(codes.InvalidArgument, "org is required")
	}
	if len(req.GetYaml()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "yaml is required")
	}
	if p.server.DB == nil {
		return nil, status.Error(codes.Unavailable, "database is not configured")
	}

	if err := p.validateOrgBlueprintYaml(req.GetOrg(), req.GetYaml()); err != nil {
		return nil, err
	}

	yamlDoc, err := blueprint.CanonicalizeRawBlueprint(req.GetYaml())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid blueprint yaml: %v", err)
	}

	name, description, _, isTemplate, err := blueprint.ParseBlueprintMeta(yamlDoc)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid blueprint yaml: %v", err)
	}
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "blueprint yaml must set a name")
	}

	if p.server.bpManager.HasGlobalBlueprint(name) {
		return nil, status.Errorf(codes.AlreadyExists,
			"blueprint '%s' already exists as a global blueprint and cannot be redefined for an org", name)
	}

	bp, err := p.server.DB.CreateBlueprint(req.GetOrg(), name, description, yamlDoc, isTemplate)
	if err != nil {
		switch {
		case errors.Is(err, dbpkg.ErrBlueprintExists):
			return nil, status.Errorf(codes.AlreadyExists, "blueprint '%s' already exists for org '%s'", name, req.GetOrg())
		case errors.Is(err, dbpkg.ErrOrgNotFound):
			return nil, status.Errorf(codes.NotFound, "organization '%s' not found", req.GetOrg())
		}
		return nil, status.Errorf(codes.Internal, "failed to create org blueprint: %v", err)
	}

	if err := p.server.bpManager.ReloadOrgBlueprints(); err != nil {
		return nil, status.Errorf(codes.Internal, "blueprint stored but failed to reload blueprint manager: %v", err)
	}

	return p.orgBlueprintToProto(bp), nil
}

// UpdateBlueprint replaces the YAML content of an existing org-scoped
// blueprint, identified by org plus the name read from the submitted YAML
// document. The description and is_template flag are refreshed from the
// document too. The YAML is validated exactly as ValidateBlueprint does
// before it is persisted, and the blueprint manager's merged view is
// refreshed so the change is immediately visible.
func (p *ProvisionerService) UpdateBlueprint(_ context.Context,
	req *provisionerv1.UpdateBlueprintRequest) (*provisionerv1.OrgBlueprint, error) {
	if req.GetOrg() == "" {
		return nil, status.Error(codes.InvalidArgument, "org is required")
	}
	if len(req.GetYaml()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "yaml is required")
	}
	if p.server.DB == nil {
		return nil, status.Error(codes.Unavailable, "database is not configured")
	}

	if err := p.validateOrgBlueprintYaml(req.GetOrg(), req.GetYaml()); err != nil {
		return nil, err
	}

	yamlDoc, err := blueprint.CanonicalizeRawBlueprint(req.GetYaml())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid blueprint yaml: %v", err)
	}

	name, description, _, isTemplate, err := blueprint.ParseBlueprintMeta(yamlDoc)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid blueprint yaml: %v", err)
	}
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "blueprint yaml must set a name")
	}

	// With expected_revision set, check the stored row's revision and then
	// swap only if that row's yaml is still what the check saw, so a
	// concurrent update in between is caught too.
	var expectedYAML []byte
	if expected := req.GetExpectedRevision(); expected != "" {
		current, err := p.server.DB.GetBlueprint(req.GetOrg(), name)
		if err != nil {
			if errors.Is(err, dbpkg.ErrBlueprintNotFound) {
				return nil, status.Errorf(codes.NotFound, "blueprint '%s' not found for org '%s'", name, req.GetOrg())
			}
			return nil, status.Errorf(codes.Internal, "failed to load org blueprint: %v", err)
		}
		revision, err := p.server.bpManager.OrgBlueprintRevision(current)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to compute current blueprint revision: %v", err)
		}
		if revision != expected {
			return nil, status.Errorf(codes.Aborted,
				"blueprint '%s' for org '%s' is at revision %s, not %s", name, req.GetOrg(), revision, expected)
		}
		expectedYAML = current.YAML
	}

	bp, err := p.server.DB.UpdateBlueprint(req.GetOrg(), name, &description, yamlDoc, isTemplate, expectedYAML)
	if err != nil {
		switch {
		case errors.Is(err, dbpkg.ErrBlueprintNotFound):
			return nil, status.Errorf(codes.NotFound, "blueprint '%s' not found for org '%s'", name, req.GetOrg())
		case errors.Is(err, dbpkg.ErrBlueprintConflict):
			return nil, status.Errorf(codes.Aborted,
				"blueprint '%s' for org '%s' was modified concurrently", name, req.GetOrg())
		}
		return nil, status.Errorf(codes.Internal, "failed to update org blueprint: %v", err)
	}

	if err := p.server.bpManager.ReloadOrgBlueprints(); err != nil {
		return nil, status.Errorf(codes.Internal, "blueprint stored but failed to reload blueprint manager: %v", err)
	}

	return p.orgBlueprintToProto(bp), nil
}

// orgBlueprintToProto converts bp to its protobuf message, filling in its
// revision. The revision is left empty if bp does not resolve.
func (p *ProvisionerService) orgBlueprintToProto(bp *models.OrgBlueprint) *provisionerv1.OrgBlueprint {
	revision, err := p.server.bpManager.OrgBlueprintRevision(bp)
	if err != nil {
		p.log.Warn().Err(err).Str("org", bp.Org).Str("blueprint", bp.Name).
			Msg("Failed to compute org blueprint revision")
	}
	bp.Revision = revision
	return gapi.OrgBlueprintToProto(bp)
}

// DeleteBlueprint removes an org-scoped blueprint from the database, and
// refreshes the blueprint manager's merged view so it stops being served.
func (p *ProvisionerService) DeleteBlueprint(_ context.Context,
	req *provisionerv1.DeleteBlueprintRequest) (*provisionerv1.DeleteBlueprintResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.GetOrg() == "" {
		return nil, status.Error(codes.InvalidArgument, "org is required")
	}
	if p.server.DB == nil {
		return nil, status.Error(codes.Unavailable, "database is not configured")
	}

	if err := p.server.DB.DeleteBlueprint(req.GetOrg(), req.GetName()); err != nil {
		if errors.Is(err, dbpkg.ErrBlueprintNotFound) {
			return nil, status.Errorf(codes.NotFound, "blueprint '%s' not found for org '%s'", req.GetName(), req.GetOrg())
		}
		return nil, status.Errorf(codes.Internal, "failed to delete org blueprint: %v", err)
	}

	if err := p.server.bpManager.ReloadOrgBlueprints(); err != nil {
		return nil, status.Errorf(codes.Internal, "blueprint deleted but failed to reload blueprint manager: %v", err)
	}

	return &provisionerv1.DeleteBlueprintResponse{Success: true}, nil
}

// validateOrgBlueprintYaml validates a submitted org blueprint document the
// same way ValidateBlueprint does, returning an InvalidArgument status
// naming the first problem found when invalid. org scopes `template:`
// resolution so an org-scoped template shadows a global one of the same name.
func (p *ProvisionerService) validateOrgBlueprintYaml(org string, yaml []byte) error {
	issues, _, err := p.server.bpManager.ValidateRawBlueprintForOrg(org, yaml)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to validate blueprint: %v", err)
	}
	if len(issues) > 0 {
		return status.Errorf(codes.InvalidArgument, "blueprint is invalid: %s (%s)", issues[0].Message, issues[0].Field)
	}
	return nil
}
