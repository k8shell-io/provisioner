// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"errors"

	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/common/pkg/gapi"
	dbpkg "github.com/k8shell-io/provisioner/internal/db"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ListOrgBlueprints returns every database-backed blueprint stored for a
// single organization.
func (p *ProvisionerService) ListOrgBlueprints(_ context.Context,
	req *provisionerv1.ListOrgBlueprintsRequest) (*provisionerv1.OrgBlueprintList, error) {
	if req.GetOrg() == "" {
		return nil, status.Error(codes.InvalidArgument, "org is required")
	}
	if p.server.DB == nil {
		return nil, status.Error(codes.Unavailable, "database is not configured")
	}

	blueprints, err := p.server.DB.ListBlueprints(req.GetOrg())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list org blueprints: %v", err)
	}

	pbBlueprints := make([]*provisionerv1.OrgBlueprint, len(blueprints))
	for i, bp := range blueprints {
		pbBlueprints[i] = gapi.OrgBlueprintToProto(bp)
	}

	return &provisionerv1.OrgBlueprintList{Blueprints: pbBlueprints}, nil
}

// GetOrgBlueprint retrieves a single org-scoped, database-backed blueprint
// by name and org.
func (p *ProvisionerService) GetOrgBlueprint(_ context.Context,
	req *provisionerv1.GetOrgBlueprintRequest) (*provisionerv1.OrgBlueprint, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.GetOrg() == "" {
		return nil, status.Error(codes.InvalidArgument, "org is required")
	}
	if p.server.DB == nil {
		return nil, status.Error(codes.Unavailable, "database is not configured")
	}

	bp, err := p.server.DB.GetBlueprint(req.GetOrg(), req.GetName())
	if err != nil {
		if errors.Is(err, dbpkg.ErrBlueprintNotFound) {
			return nil, status.Errorf(codes.NotFound, "blueprint '%s' not found for org '%s'", req.GetName(), req.GetOrg())
		}
		return nil, status.Errorf(codes.Internal, "failed to get org blueprint: %v", err)
	}

	return gapi.OrgBlueprintToProto(bp), nil
}

// CreateOrgBlueprint stores a new org-scoped blueprint in the database. The
// submitted YAML is validated exactly as ValidateBlueprint does before it is
// persisted, and the blueprint manager's merged view is refreshed so the new
// blueprint is immediately usable.
func (p *ProvisionerService) CreateOrgBlueprint(_ context.Context,
	req *provisionerv1.CreateOrgBlueprintRequest) (*provisionerv1.OrgBlueprint, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.GetOrg() == "" {
		return nil, status.Error(codes.InvalidArgument, "org is required")
	}
	if p.server.DB == nil {
		return nil, status.Error(codes.Unavailable, "database is not configured")
	}

	if err := p.validateOrgBlueprintYaml(req.GetYaml()); err != nil {
		return nil, err
	}

	bp, err := p.server.DB.CreateBlueprint(req.GetOrg(), req.GetName(), req.GetDescription(), req.GetYaml(), req.GetIsTemplate())
	if err != nil {
		switch {
		case errors.Is(err, dbpkg.ErrBlueprintExists):
			return nil, status.Errorf(codes.AlreadyExists, "blueprint '%s' already exists for org '%s'", req.GetName(), req.GetOrg())
		case errors.Is(err, dbpkg.ErrOrgNotFound):
			return nil, status.Errorf(codes.NotFound, "organization '%s' not found", req.GetOrg())
		}
		return nil, status.Errorf(codes.Internal, "failed to create org blueprint: %v", err)
	}

	if err := p.server.bpManager.ReloadOrgBlueprints(); err != nil {
		return nil, status.Errorf(codes.Internal, "blueprint stored but failed to reload blueprint manager: %v", err)
	}

	return gapi.OrgBlueprintToProto(bp), nil
}

// UpdateOrgBlueprint replaces the YAML content (and optionally the
// description) of an existing org-scoped blueprint, identified by name and
// org. The submitted YAML is validated exactly as ValidateBlueprint does
// before it is persisted, and the blueprint manager's merged view is
// refreshed so the change is immediately visible.
func (p *ProvisionerService) UpdateOrgBlueprint(_ context.Context,
	req *provisionerv1.UpdateOrgBlueprintRequest) (*provisionerv1.OrgBlueprint, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.GetOrg() == "" {
		return nil, status.Error(codes.InvalidArgument, "org is required")
	}
	if len(req.GetYaml()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "yaml is required")
	}
	if p.server.DB == nil {
		return nil, status.Error(codes.Unavailable, "database is not configured")
	}

	if err := p.validateOrgBlueprintYaml(req.GetYaml()); err != nil {
		return nil, err
	}

	var description *string
	if req.Description != nil {
		description = &req.Description.Value
	}

	bp, err := p.server.DB.UpdateBlueprint(req.GetOrg(), req.GetName(), description, req.GetYaml())
	if err != nil {
		if errors.Is(err, dbpkg.ErrBlueprintNotFound) {
			return nil, status.Errorf(codes.NotFound, "blueprint '%s' not found for org '%s'", req.GetName(), req.GetOrg())
		}
		return nil, status.Errorf(codes.Internal, "failed to update org blueprint: %v", err)
	}

	if err := p.server.bpManager.ReloadOrgBlueprints(); err != nil {
		return nil, status.Errorf(codes.Internal, "blueprint stored but failed to reload blueprint manager: %v", err)
	}

	return gapi.OrgBlueprintToProto(bp), nil
}

// DeleteOrgBlueprint removes an org-scoped blueprint from the database, and
// refreshes the blueprint manager's merged view so it stops being served.
func (p *ProvisionerService) DeleteOrgBlueprint(_ context.Context,
	req *provisionerv1.DeleteOrgBlueprintRequest) (*provisionerv1.DeleteOrgBlueprintResponse, error) {
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

	return &provisionerv1.DeleteOrgBlueprintResponse{Success: true}, nil
}

// validateOrgBlueprintYaml validates a submitted org blueprint document the
// same way ValidateBlueprint does, returning an InvalidArgument status
// naming the first problem found when invalid.
func (p *ProvisionerService) validateOrgBlueprintYaml(yaml []byte) error {
	issues, _, err := p.server.bpManager.ValidateRawBlueprint(yaml)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to validate blueprint: %v", err)
	}
	if len(issues) > 0 {
		return status.Errorf(codes.InvalidArgument, "blueprint is invalid: %s (%s)", issues[0].Message, issues[0].Field)
	}
	return nil
}
