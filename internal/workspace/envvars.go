// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"fmt"

	identityv1 "github.com/k8shell-io/common/pkg/api/gen/go/identity/v1"
)

// FetchUserEnvVars retrieves the effective environment variables for the
// workspace's owning user from the Identity service (org-level variables
// overridden by any user-owned variable with the same key) and stores them
// on the workspace so Values() surfaces them to the workspace pod. Secret
// values come back redacted from ListUserEnvVars, so each is re-fetched in
// full via GetUserEnvVar. It is a no-op, not an error, when identityClient
// or the workspace's user is unset. Values() applies these vars for both
// standalone and injected provisioning, since both share the same call.
func (w *Workspace) FetchUserEnvVars(ctx context.Context) error {
	if w.identify == nil || w.user == nil {
		return nil
	}

	resp, err := w.identify.ListUserEnvVars(ctx, &identityv1.ListUserEnvVarsRequest{
		Username: w.user.Username,
	})
	if err != nil {
		return fmt.Errorf("failed to list environment variables for user %s: %w", w.user.Username, err)
	}

	vars := make(map[string]string, len(resp.GetEnvVars()))
	for _, ev := range resp.GetEnvVars() {
		value := ev.GetValue()
		if ev.GetRedacted() {
			full, err := w.identify.GetUserEnvVar(ctx, &identityv1.GetUserEnvVarRequest{
				Username: w.user.Username,
				Key:      ev.GetKey(),
			})
			if err != nil {
				return fmt.Errorf("failed to fetch secret environment variable %s for user %s: %w",
					ev.GetKey(), w.user.Username, err)
			}
			value = full.GetValue()
		}
		vars[ev.GetKey()] = value
	}

	w.userEnvVars = vars
	return nil
}
