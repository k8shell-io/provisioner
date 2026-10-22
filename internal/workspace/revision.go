// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import "github.com/k8shell-io/common/pkg/models"

// BlueprintRevisionFunc returns the current revision of the named blueprint
// as seen by a user of org, or "" if it does not exist. It is satisfied by
// (*blueprint.BlueprintManager).GetBlueprintRevision.
type BlueprintRevisionFunc func(org, blueprint string) string

// MarkBlueprintOutdated sets BlueprintOutdated on each workspace by comparing
// its recorded BlueprintRevision with the blueprint's current revision.
// Each distinct (organization, blueprint) pair is resolved once per call, so
// listing many workspaces of the same blueprint costs a single lookup.
//
// BlueprintOutdated is left nil when it cannot be determined: the workspace
// carries no revision, the blueprint no longer resolves, or the workspace was
// provisioned from a repository's k8shell file. Only the repo form of a
// userstr sets RepoName, and for it the recorded revision is that of the
// file composed with its template, not of the template named in Blueprint,
// so comparing the two would be meaningless.
func MarkBlueprintOutdated(workspaces []*models.WorkspaceDetails, current BlueprintRevisionFunc) {
	if current == nil {
		return
	}
	type key struct{ org, blueprint string }
	memo := make(map[key]string)

	for _, w := range workspaces {
		if w == nil || w.BlueprintRevision == "" || w.Blueprint == "" || w.RepoName != "" {
			continue
		}
		k := key{w.Organization, w.Blueprint}
		rev, ok := memo[k]
		if !ok {
			rev = current(k.org, k.blueprint)
			memo[k] = rev
		}
		if rev == "" {
			continue
		}
		outdated := rev != w.BlueprintRevision
		w.BlueprintOutdated = &outdated
	}
}
