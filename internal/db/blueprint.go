// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/k8shell-io/common/pkg/models"
)

// ErrBlueprintExists is returned by CreateBlueprint when a blueprint with
// the same (org, name) already exists.
var ErrBlueprintExists = errors.New("org blueprint already exists")

// ErrBlueprintNotFound is returned by GetBlueprint/UpdateBlueprint/
// DeleteBlueprint when no blueprint with the given (org, name) exists.
var ErrBlueprintNotFound = errors.New("org blueprint not found")

// ErrOrgNotFound is returned by CreateBlueprint when org does not exist in
// identity.organizations (see the FK on org_blueprints.org).
var ErrOrgNotFound = errors.New("organization not found")

// CreateBlueprint registers a new org-scoped blueprint.
func (d *DB) CreateBlueprint(org, name, description string, yaml []byte, isTemplate bool) (*models.OrgBlueprint, error) {
	bp := &models.OrgBlueprint{Org: org, Name: name, Description: description, YAML: yaml, IsTemplate: isTemplate}
	err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO provisioner.org_blueprints (org, name, description, yaml, is_template)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING created_at, updated_at`,
		org, name, description, yaml, isTemplate,
	).Scan(&bp.CreatedAt, &bp.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch {
			case pgErr.Code == "23505" && pgErr.ConstraintName == "org_blueprints_pkey":
				return nil, fmt.Errorf("%w: org %q name %q", ErrBlueprintExists, org, name)
			case pgErr.Code == "23503" && pgErr.ConstraintName == "org_blueprints_org_fkey":
				return nil, fmt.Errorf("%w: %q", ErrOrgNotFound, org)
			}
		}
		return nil, fmt.Errorf("insert org blueprint: %w", err)
	}

	return bp, nil
}

// UpdateBlueprint replaces an existing org blueprint's yaml content, and
// optionally its description. name and org together identify the blueprint
// and are immutable; pass a nil description to leave it unchanged. Returns
// ErrBlueprintNotFound when no blueprint with that (org, name) exists.
func (d *DB) UpdateBlueprint(org, name string, description *string, yaml []byte) (*models.OrgBlueprint, error) {
	bp := &models.OrgBlueprint{Org: org, Name: name}
	err := d.Pool.QueryRow(context.Background(),
		`UPDATE provisioner.org_blueprints
		 SET description = COALESCE($3, description), yaml = $4, updated_at = NOW()
		 WHERE org=$1 AND name=$2
		 RETURNING COALESCE(description, ''), yaml, is_template, created_at, updated_at`,
		org, name, description, yaml,
	).Scan(&bp.Description, &bp.YAML, &bp.IsTemplate, &bp.CreatedAt, &bp.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: org %q name %q", ErrBlueprintNotFound, org, name)
	}
	if err != nil {
		return nil, fmt.Errorf("update org blueprint: %w", err)
	}

	return bp, nil
}

// GetBlueprint retrieves a single org blueprint by org and name. Returns
// ErrBlueprintNotFound when no blueprint with that (org, name) exists.
func (d *DB) GetBlueprint(org, name string) (*models.OrgBlueprint, error) {
	bp := &models.OrgBlueprint{Org: org, Name: name}
	err := d.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(description, ''), yaml, is_template, created_at, updated_at
		 FROM provisioner.org_blueprints
		 WHERE org=$1 AND name=$2`,
		org, name,
	).Scan(&bp.Description, &bp.YAML, &bp.IsTemplate, &bp.CreatedAt, &bp.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: org %q name %q", ErrBlueprintNotFound, org, name)
	}
	if err != nil {
		return nil, fmt.Errorf("get org blueprint: %w", err)
	}

	return bp, nil
}

// LookupBlueprint behaves like GetBlueprint but returns (nil, nil) instead of
// ErrBlueprintNotFound when no row with that (org, name) exists, for callers
// that treat "absent" as a normal outcome rather than an error.
func (d *DB) LookupBlueprint(org, name string) (*models.OrgBlueprint, error) {
	bp, err := d.GetBlueprint(org, name)
	if errors.Is(err, ErrBlueprintNotFound) {
		return nil, nil
	}
	return bp, err
}

// ListBlueprints returns every blueprint stored for a single org, ordered by name.
func (d *DB) ListBlueprints(org string) ([]*models.OrgBlueprint, error) {
	rows, err := d.Pool.Query(context.Background(),
		`SELECT org, name, COALESCE(description, ''), yaml, is_template, created_at, updated_at
		 FROM provisioner.org_blueprints
		 WHERE org=$1
		 ORDER BY name`,
		org,
	)
	if err != nil {
		return nil, fmt.Errorf("list org blueprints: %w", err)
	}
	defer rows.Close()

	return scanBlueprints(rows)
}

// ListAllBlueprints returns every org blueprint across every org, ordered by
// org then name. Used by the blueprint manager to build its merged,
// file-plus-database view on load and on every reload.
func (d *DB) ListAllBlueprints() ([]*models.OrgBlueprint, error) {
	rows, err := d.Pool.Query(context.Background(),
		`SELECT org, name, COALESCE(description, ''), yaml, is_template, created_at, updated_at
		 FROM provisioner.org_blueprints
		 ORDER BY org, name`,
	)
	if err != nil {
		return nil, fmt.Errorf("list all org blueprints: %w", err)
	}
	defer rows.Close()

	return scanBlueprints(rows)
}

func scanBlueprints(rows pgx.Rows) ([]*models.OrgBlueprint, error) {
	var blueprints []*models.OrgBlueprint
	for rows.Next() {
		var bp models.OrgBlueprint
		if err := rows.Scan(&bp.Org, &bp.Name, &bp.Description, &bp.YAML, &bp.IsTemplate, &bp.CreatedAt, &bp.UpdatedAt); err != nil {
			return nil, err
		}
		blueprints = append(blueprints, &bp)
	}
	return blueprints, rows.Err()
}

// DeleteBlueprint removes an org blueprint from the registry. Returns
// ErrBlueprintNotFound when no blueprint with that (org, name) exists.
func (d *DB) DeleteBlueprint(org, name string) error {
	result, err := d.Pool.Exec(context.Background(),
		`DELETE FROM provisioner.org_blueprints WHERE org=$1 AND name=$2`, org, name)
	if err != nil {
		return fmt.Errorf("delete org blueprint: %w", err)
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("%w: org %q name %q", ErrBlueprintNotFound, org, name)
	}

	return nil
}
