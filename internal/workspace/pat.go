// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"fmt"

	"github.com/k8shell-io/common/pkg/api/client/identity"
	identityv1 "github.com/k8shell-io/common/pkg/api/gen/go/identity/v1"
	"github.com/k8shell-io/provisioner/internal/helm"
)

// DeleteWorkspacePAT deletes the personal access token minted for a
// workspace, identified by the canonical ID stored as its token Name under
// the given username. It is a no-op, not an error, when identityClient is
// nil, username/canonicalId are empty, or no matching token is found — PAT
// deletion is always best-effort and must never block workspace teardown.
func DeleteWorkspacePAT(ctx context.Context, identityClient *identity.IdentityClient, username, canonicalId string) error {
	if identityClient == nil || username == "" || canonicalId == "" {
		return nil
	}

	resp, err := identityClient.ListAccessTokens(ctx, &identityv1.Username{Username: username})
	if err != nil {
		return fmt.Errorf("failed to list access tokens for user %s: %w", username, err)
	}

	for _, t := range resp.GetTokens() {
		if t.GetName() != canonicalId {
			continue
		}
		if _, err := identityClient.DeleteAccessToken(ctx, &identityv1.DeleteAccessTokenRequest{
			Id: t.GetId(),
		}); err != nil {
			return fmt.Errorf("failed to delete access token %d for workspace %s: %w", t.GetId(), canonicalId, err)
		}
		return nil
	}

	return nil
}

// MintPAT creates a fresh personal access token for the workspace's owning
// user via the Identity service, stores it on the workspace (see SetPAT) so
// it is picked up by Values()/RefreshPATSecret, and returns the token. Used
// before (re)creating a workspace pod, since the previous token is deleted
// when a workspace is stopped (see DeletePAT).
func (w *Workspace) MintPAT(ctx context.Context, scopes []string) (string, error) {
	resp, err := w.identify.CreateAccessToken(ctx, &identityv1.CreateAccessTokenRequest{
		Username: w.user.Username,
		Name:     w.canonicalId,
		Scopes:   scopes,
		Renew:    true,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create PAT for workspace %s: %w", w.Name, err)
	}
	w.SetPAT(resp.GetToken())
	return resp.GetToken(), nil
}

// DeleteWorkspacePATFromLabels deletes a workspace's PAT using the
// username/canonical-id labels already present on its pod (or injected
// workload pod template). It is used by cleanup paths that intentionally
// avoid a full identity lookup, e.g. bulk user-workspace deletion that runs
// after the owning user may already be gone.
func DeleteWorkspacePATFromLabels(ctx context.Context, identityClient *identity.IdentityClient, labels map[string]string) error {
	if labels == nil {
		return nil
	}
	return DeleteWorkspacePAT(ctx, identityClient, labels[helm.LabelUsername], labels[helm.LabelCanonicalId])
}
