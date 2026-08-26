// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"fmt"

	"github.com/k8shell-io/common/pkg/models"
	"gopkg.in/yaml.v3"
)

// OrgBlueprintStore supplies org-scoped blueprint definitions persisted in
// the database. BlueprintManager merges them with the file-based blueprints
// loaded from disk: an org blueprint takes precedence over a file-based
// blueprint of the same name for a user belonging to that org (see
// lookupRawBlueprint), while every other org sees only the file-based one.
type OrgBlueprintStore interface {
	// ListAllBlueprints returns every org blueprint across every org. Called
	// on every (re)load, mirroring how loadRawBlueprints re-walks the file
	// tree on every reload.
	ListAllBlueprints() ([]*models.OrgBlueprint, error)
}

// orgBlueprintKey is the bm.rawBlueprints key an org blueprint is stored
// under: namespaced by org so it can coexist with a file-based blueprint of
// the same bare name without colliding.
func orgBlueprintKey(org, name string) string {
	return org + "/" + name
}

// lookupRawBlueprint resolves name to its RawBlueprint, preferring an
// org-scoped database blueprint of the same name over the file-based/global
// one when scope carries a user belonging to an organization. Callers must
// hold bm.mu for reading.
func (bm *BlueprintManager) lookupRawBlueprint(name string, scope *BlueprintScope) (*RawBlueprint, bool) {
	if scope != nil && scope.User != nil && scope.User.Organization != "" {
		if rawBp, ok := bm.rawBlueprints[orgBlueprintKey(scope.User.Organization, name)]; ok {
			return rawBp, true
		}
	}
	rawBp, ok := bm.rawBlueprints[name]
	return rawBp, ok
}

// loadOrgBlueprints fetches every org blueprint from bm.orgStore and inserts
// each into bm.rawBlueprints under its org-scoped key (orgBlueprintKey),
// alongside the file-based blueprints loadRawBlueprints already populated.
// A no-op when no store is configured (org blueprint support disabled).
// Must be called as part of the same single-threaded reload pass as
// loadRawBlueprints, before resolveInheritance runs.
func (bm *BlueprintManager) loadOrgBlueprints() error {
	if bm.orgStore == nil {
		return nil
	}

	orgBlueprints, err := bm.orgStore.ListAllBlueprints()
	if err != nil {
		return fmt.Errorf("failed to load org blueprints from database: %w", err)
	}

	for _, ob := range orgBlueprints {
		var doc yaml.Node
		if err := yaml.Unmarshal(ob.YAML, &doc); err != nil {
			return fmt.Errorf("org blueprint %q (org %q): invalid yaml: %w", ob.Name, ob.Org, err)
		}

		node, err := bm.resolveValidationNode(&doc)
		if err != nil {
			return fmt.Errorf("org blueprint %q (org %q): %w", ob.Name, ob.Org, err)
		}

		var bpData map[string]interface{}
		if err := node.Decode(&bpData); err != nil {
			bpData = make(map[string]interface{})
		}
		template, _ := bpData["template"].(string)

		key := orgBlueprintKey(ob.Org, ob.Name)
		if existing, exists := bm.rawBlueprints[key]; exists {
			return fmt.Errorf("duplicate org blueprint %q for org %q: already defined in %s", ob.Name, ob.Org, existing.SourceFile)
		}

		bm.rawBlueprints[key] = &RawBlueprint{
			Name:        ob.Name,
			Org:         ob.Org,
			Description: ob.Description,
			Template:    template,
			IsTemplate:  ob.IsTemplate,
			SourceFile:  fmt.Sprintf("db:%s/%s", ob.Org, ob.Name),
			Node:        node,
		}
	}

	return nil
}

// ReloadOrgBlueprints refreshes the manager's merged file-plus-database view
// after an org blueprint is created, updated, or deleted, so the change is
// immediately visible to GetBlueprint. It reruns the full load pipeline
// (the same one a file watcher-triggered reload uses), since org and
// file-based blueprints share the same inheritance resolution and
// validation pass.
func (bm *BlueprintManager) ReloadOrgBlueprints() error {
	return bm.loadAndValidateBlueprints()
}
