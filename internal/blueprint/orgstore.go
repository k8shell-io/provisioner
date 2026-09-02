// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"fmt"
	"strings"

	"github.com/k8shell-io/common/pkg/models"
	"gopkg.in/yaml.v3"
)

// OrgBlueprintStore supplies org-scoped blueprint definitions persisted in
// the database. An org-scoped lookup is served from the store on every RPC
// (see lookupOrgFromStore); an org blueprint takes precedence over a
// file-based blueprint of the same name for a user belonging to that org,
// while every other org sees only the file-based one. The file-based
// blueprints loaded from disk stay served from the in-memory cache.
type OrgBlueprintStore interface {
	// ListAllBlueprints returns every org blueprint across every org. Called
	// on every (re)load, mirroring how loadRawBlueprints re-walks the file
	// tree on every reload.
	ListAllBlueprints() ([]*models.OrgBlueprint, error)
	// LookupBlueprint returns the org blueprint identified by (org, name), or
	// (nil, nil) when no such row exists. Used to serve org-scoped blueprints
	// straight from the database on every RPC, so a row created, changed or
	// deleted out of band is reflected without waiting for a full reload.
	LookupBlueprint(org, name string) (*models.OrgBlueprint, error)
}

// ParseBlueprintMeta reads the identifying metadata — name, description,
// parent template and isTemplate — from a raw blueprint YAML document,
// applying the same top-level `blueprint:` unwrapping rule the loader uses.
// It does not validate the document (callers do that separately via
// ValidateRawBlueprint); it only extracts the fields the database row is
// keyed and filtered by.
func ParseBlueprintMeta(data []byte) (name, description, template string, isTemplate bool, err error) {
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil {
		return "", "", "", false, err
	}

	node := &doc
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}

	var bpData map[string]interface{}
	if err = node.Decode(&bpData); err != nil {
		return "", "", "", false, fmt.Errorf("blueprint document is not a mapping: %w", err)
	}
	if inner, ok := bpData["blueprint"].(map[string]interface{}); ok {
		bpData = inner
	}

	name, _ = bpData["name"].(string)
	description, _ = bpData["description"].(string)
	description = strings.Join(strings.Fields(description), " ")
	template, _ = bpData["template"].(string)
	isTemplate, _ = bpData["isTemplate"].(bool)
	return name, description, template, isTemplate, nil
}

// orgBlueprintKey is the bm.rawBlueprints key an org blueprint is stored
// under: namespaced by org so it can coexist with a file-based blueprint of
// the same bare name without colliding.
func orgBlueprintKey(org, name string) string {
	return org + "/" + name
}

// buildOrgRawBlueprint turns one database row into a fully resolved
// RawBlueprint: its YAML is parsed, CEL expressions are normalised back to
// `!cel` tags, and — when it names a `template:` — merged with that
// file-based template's already-resolved definition. It is the per-request
// counterpart of loadOrgBlueprints + resolveInheritance, used to serve an
// org blueprint straight from the store without consulting the shared cache.
// Callers must NOT hold bm.mu.
func (bm *BlueprintManager) buildOrgRawBlueprint(ob *models.OrgBlueprint) (*RawBlueprint, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(ob.YAML, &doc); err != nil {
		return nil, fmt.Errorf("org blueprint %q (org %q): invalid yaml: %w", ob.Name, ob.Org, err)
	}

	own, err := bm.resolveValidationNode(&doc)
	if err != nil {
		return nil, fmt.Errorf("org blueprint %q (org %q): %w", ob.Name, ob.Org, err)
	}
	own = restoreCELNodes(own)

	var bpData map[string]interface{}
	if err := own.Decode(&bpData); err != nil {
		bpData = map[string]interface{}{}
	}
	template, _ := bpData["template"].(string)

	rb := &RawBlueprint{
		Name:        ob.Name,
		Org:         ob.Org,
		Description: ob.Description,
		Template:    template,
		IsTemplate:  ob.IsTemplate,
		SourceFile:  fmt.Sprintf("db:%s/%s", ob.Org, ob.Name),
		CreatedAt:   ob.CreatedAt,
		UpdatedAt:   ob.UpdatedAt,
		Node:        own,
		OwnNode:     own,
	}
	if template == "" {
		rb.InheritanceChain = []string{ob.Name}
		return rb, nil
	}

	bm.mu.RLock()
	parent, ok := bm.rawBlueprints[template]
	bm.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("cannot find template %q for org blueprint %q", template, ob.Name)
	}

	merged, err := bm.mergeYAMLNodes(parent.Node, own)
	if err != nil {
		return nil, fmt.Errorf("failed to merge template %q for org blueprint %q: %w", template, ob.Name, err)
	}
	rb.Node = merged
	rb.InheritanceChain = append(append([]string{}, parent.InheritanceChain...), ob.Name)
	return rb, nil
}

// lookupOrgFromStore fetches (org, name) straight from the backing store and
// resolves it via buildOrgRawBlueprint. Returns ok=false with no error when
// the manager has no store, org is empty, or the row does not exist — the
// caller then falls back to the file-based blueprint of that name, if any.
// Callers must NOT hold bm.mu.
func (bm *BlueprintManager) lookupOrgFromStore(org, name string) (*RawBlueprint, bool, error) {
	if org == "" || bm.orgStore == nil {
		return nil, false, nil
	}
	ob, err := bm.orgStore.LookupBlueprint(org, name)
	if err != nil {
		return nil, false, fmt.Errorf("load org blueprint %q/%q from store: %w", org, name, err)
	}
	if ob == nil {
		return nil, false, nil
	}
	rb, err := bm.buildOrgRawBlueprint(ob)
	if err != nil {
		return nil, false, err
	}
	return rb, true, nil
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
		// Rows written before CEL was stored canonically hold the expression
		// as a plain "!cel:"/"!cel " string; convert it back to a real `!cel`
		// tag so it evaluates instead of being used as a literal value.
		node = restoreCELNodes(node)

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
			CreatedAt:   ob.CreatedAt,
			UpdatedAt:   ob.UpdatedAt,
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
