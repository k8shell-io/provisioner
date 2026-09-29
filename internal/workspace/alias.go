// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/common/pkg/validator"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/rs/zerolog"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	// ErrWebProxyAliasHeld is returned when a web-proxy alias is already
	// held by another workspace of the same organization.
	ErrWebProxyAliasHeld = errors.New("web-proxy alias is held by another workspace")
	// ErrNoWebProxyRoute is returned when a web-proxy alias is set on a
	// workspace that publishes no web-proxy route.
	ErrNoWebProxyRoute = errors.New("workspace publishes no web-proxy route")
)

const (
	aliasClaimPrefix       = "web-proxy-alias-"
	aliasClaimKeyOrg       = "organization"
	aliasClaimKeyAlias     = "alias"
	aliasClaimKeyWorkspace = "workspace"
	aliasClaimKeyClaimedAt = "claimedAt"

	// staleAliasClaimGrace is how long a claim is protected after it was
	// taken, even if its holder has no Helm release. A workspace claims its
	// alias before installing its release, so without the grace a concurrent
	// provision could see the release missing and take the claim over.
	staleAliasClaimGrace = 5 * time.Minute

	// aliasClaimAttempts bounds the create/take-over loop in claim when it
	// races with other claimers of the same alias.
	aliasClaimAttempts = 3
)

// aliasHeldError reports the workspace holding a contested alias.
type aliasHeldError struct {
	alias  string
	holder string
}

func (e *aliasHeldError) Error() string {
	return fmt.Sprintf("alias %q is held by workspace %s", e.alias, e.holder)
}

func (e *aliasHeldError) Is(target error) bool { return target == ErrWebProxyAliasHeld }

// aliasClaims manages the ConfigMaps that make web-proxy aliases unique per
// organization. A claim's name is derived from (organization, alias), so the
// API server's atomic Create is the collision signal. Claims carry the
// holder's canonical-id label, so Workspace.Uninstall's label sweep frees
// them together with the rest of the workspace.
type aliasClaims struct {
	kube      kubernetes.Interface
	namespace string
	log       *zerolog.Logger
	// holderExists reports whether the named workspace still exists. A claim
	// whose holder is gone (and is older than staleAliasClaimGrace) is stale
	// and may be taken over.
	holderExists func(ctx context.Context, workspace string) (bool, error)
	now          func() time.Time
}

func newAliasClaims(helmClient *helm.Client, log *zerolog.Logger) *aliasClaims {
	return &aliasClaims{
		kube:      helmClient.KubeClient(),
		namespace: helmClient.TargetNamespace(),
		log:       log,
		holderExists: func(_ context.Context, workspace string) (bool, error) {
			return releaseExists(helmClient, workspace)
		},
		now: time.Now,
	}
}

// aliasClaimName returns the claim ConfigMap name for alias within org.
// Organization names are label values and cannot contain "/", so the hashed
// key is unambiguous.
func aliasClaimName(org, alias string) string {
	sum := sha256.Sum256([]byte(org + "/" + alias))
	return aliasClaimPrefix + hex.EncodeToString(sum[:16])
}

func (c *aliasClaims) newClaim(name, org, alias, workspace, canonicalID string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "k8shell-provisioner",
				helm.LabelWebProxyAliasClaim:   "true",
				helm.LabelWebProxyAlias:        alias,
				helm.LabelOrganization:         org,
				helm.LabelCanonicalId:          canonicalID,
			},
		},
		Data: map[string]string{
			aliasClaimKeyOrg:       org,
			aliasClaimKeyAlias:     alias,
			aliasClaimKeyWorkspace: workspace,
			aliasClaimKeyClaimedAt: c.now().UTC().Format(time.RFC3339),
		},
	}
}

// claim takes alias within org for workspace and returns the claim's name.
// Claiming an alias the workspace already holds succeeds. It returns an
// error matching ErrWebProxyAliasHeld when another live workspace holds it.
func (c *aliasClaims) claim(ctx context.Context, org, alias, workspace, canonicalID string) (string, error) {
	name := aliasClaimName(org, alias)
	cms := c.kube.CoreV1().ConfigMaps(c.namespace)

	for range aliasClaimAttempts {
		_, err := cms.Create(ctx, c.newClaim(name, org, alias, workspace, canonicalID), metav1.CreateOptions{})
		if err == nil {
			return name, nil
		}
		if !k8serrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("failed to create web-proxy alias claim %s: %w", name, err)
		}

		existing, err := cms.Get(ctx, name, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			continue // released since our Create; try again
		}
		if err != nil {
			return "", fmt.Errorf("failed to get web-proxy alias claim %s: %w", name, err)
		}

		holder := existing.Data[aliasClaimKeyWorkspace]
		if holder == workspace {
			return name, nil
		}
		stale, err := c.isStale(ctx, existing)
		if err != nil {
			return "", err
		}
		if !stale {
			return "", &aliasHeldError{alias: alias, holder: holder}
		}

		// Take over the stale claim. The resourceVersion makes the update
		// fail with a conflict if anyone else touched it since our Get.
		takeover := c.newClaim(name, org, alias, workspace, canonicalID)
		takeover.ResourceVersion = existing.ResourceVersion
		_, err = cms.Update(ctx, takeover, metav1.UpdateOptions{})
		if err == nil {
			return name, nil
		}
		if !k8serrors.IsConflict(err) && !k8serrors.IsNotFound(err) {
			return "", fmt.Errorf("failed to take over web-proxy alias claim %s: %w", name, err)
		}
	}
	return "", fmt.Errorf("failed to claim web-proxy alias %q: too many concurrent claimers", alias)
}

// isStale reports whether claim may be taken over: it is past the grace
// period and its holder workspace no longer exists.
func (c *aliasClaims) isStale(ctx context.Context, claim *corev1.ConfigMap) (bool, error) {
	if at, err := time.Parse(time.RFC3339, claim.Data[aliasClaimKeyClaimedAt]); err == nil &&
		c.now().Sub(at) < staleAliasClaimGrace {
		return false, nil
	}
	holder := claim.Data[aliasClaimKeyWorkspace]
	if holder == "" {
		return true, nil
	}
	exists, err := c.holderExists(ctx, holder)
	if err != nil {
		return false, fmt.Errorf("failed to check holder %s of web-proxy alias claim %s: %w", holder, claim.Name, err)
	}
	return !exists, nil
}

// release deletes every alias claim held by the workspace with canonicalID,
// except the claim named keep (pass "" to release all).
func (c *aliasClaims) release(ctx context.Context, canonicalID, keep string) error {
	cms := c.kube.CoreV1().ConfigMaps(c.namespace)
	list, err := cms.List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=true,%s=%s", helm.LabelWebProxyAliasClaim, helm.LabelCanonicalId, canonicalID),
	})
	if err != nil {
		return fmt.Errorf("failed to list web-proxy alias claims: %w", err)
	}
	var errs []error
	for i := range list.Items {
		cm := &list.Items[i]
		if cm.Name == keep {
			continue
		}
		// The precondition keeps us from deleting a claim a concurrent
		// take-over has just handed to another workspace.
		rv := cm.ResourceVersion
		err := cms.Delete(ctx, cm.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &rv}})
		if err != nil && !k8serrors.IsNotFound(err) && !k8serrors.IsConflict(err) {
			errs = append(errs, fmt.Errorf("failed to release web-proxy alias claim %s: %w", cm.Name, err))
		}
	}
	return errors.Join(errs...)
}

// holder returns the workspace holding alias within org.
func (c *aliasClaims) holder(ctx context.Context, org, alias string) (string, bool, error) {
	cm, err := c.kube.CoreV1().ConfigMaps(c.namespace).Get(ctx, aliasClaimName(org, alias), metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("failed to get web-proxy alias claim: %w", err)
	}
	// Guard against a hash collision between two (org, alias) pairs.
	if cm.Data[aliasClaimKeyOrg] != org || cm.Data[aliasClaimKeyAlias] != alias {
		return "", false, nil
	}
	holder := cm.Data[aliasClaimKeyWorkspace]
	return holder, holder != "", nil
}

// FindWorkspaceByAlias returns the workspace holding the web-proxy alias
// within org. It returns models.ErrWorkspaceNotFound when no workspace holds
// it, including when the holder's live alias no longer matches the claim.
func FindWorkspaceByAlias(ctx context.Context, helmClient *helm.Client, org, alias string) (*models.WorkspaceDetails, error) {
	holder, found, err := newAliasClaims(helmClient, nil).holder(ctx, org, alias)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: alias %s in organization %s", models.ErrWorkspaceNotFound, alias, org)
	}
	// Aliases are only applied to standalone workspaces, so the injection
	// namespaces need not be searched.
	details, _, err := FindWorkspace(ctx, helmClient, holder, nil, true)
	if err != nil {
		return nil, err
	}
	if details.WebProxyAlias != alias || details.Organization != org {
		return nil, fmt.Errorf("%w: alias %s in organization %s", models.ErrWorkspaceNotFound, alias, org)
	}
	return details, nil
}

// blueprintWebProxyAlias returns the alias the workspace's blueprint asks
// for, or "" when it declares no web-proxy route or no alias.
func (w *Workspace) blueprintWebProxyAlias() string {
	if w.blueprint == nil || w.blueprint.Network.WebProxy == nil {
		return ""
	}
	return strings.TrimSpace(w.blueprint.Network.WebProxy.Alias)
}

// reconcile makes the workspace hold exactly the claim for desired within
// org (none when desired is "") and returns the alias to apply. An invalid
// or contested alias is not an error: the workspace goes without one and
// message says why. Only failures to talk to the API server are returned as
// errors; failing to release an old claim is logged, not fatal.
func (c *aliasClaims) reconcile(ctx context.Context, org, workspace, canonicalID, desired string) (applied, message string, err error) {
	keep := ""
	switch {
	case desired == "":
	case !validator.IsWebProxyAlias(desired):
		message = fmt.Sprintf("alias %q is not a valid DNS label; not applied", desired)
	default:
		name, claimErr := c.claim(ctx, org, desired, workspace, canonicalID)
		var held *aliasHeldError
		switch {
		case errors.As(claimErr, &held):
			message = held.Error() + "; not applied"
		case claimErr != nil:
			return "", "", claimErr
		default:
			applied, keep = desired, name
		}
	}

	if relErr := c.release(ctx, canonicalID, keep); relErr != nil && c.log != nil {
		c.log.Error().Err(relErr).Msgf("Failed to release stale web-proxy alias claims of workspace %s", workspace)
	}
	return applied, message, nil
}

// claimForUpdate claims the alias an update sets on the workspace running as
// pod and returns the claim's name, or "" when the update clears the alias.
// The workspace must publish a web-proxy route, either on the pod already or
// through the same update (ErrNoWebProxyRoute otherwise); another workspace
// of the organization holding the alias yields ErrWebProxyAliasHeld.
func (c *aliasClaims) claimForUpdate(ctx context.Context, pod *corev1.Pod, opts UpdateOptions, canonicalID string) (string, error) {
	alias := opts.WebProxyAlias
	if alias == "" {
		return "", nil
	}
	if !validator.IsWebProxyAlias(alias) {
		return "", fmt.Errorf("%w: alias %q is not a valid DNS label", models.ErrInvalidParameters, alias)
	}
	hasRoute := pod.Annotations[helm.AnnotationWebProxyPort] != ""
	if opts.ReplaceWebProxy {
		hasRoute = opts.WebProxyPort > 0
	}
	if !hasRoute {
		return "", fmt.Errorf("%w: cannot set alias %q on workspace %s", ErrNoWebProxyRoute, alias, pod.Name)
	}
	return c.claim(ctx, pod.Labels[helm.LabelOrganization], alias, pod.Name, canonicalID)
}

// reconcileWebProxyAlias reconciles the workspace's alias claim with
// desired; see aliasClaims.reconcile.
func (w *Workspace) reconcileWebProxyAlias(ctx context.Context, desired string) (applied, message string, err error) {
	applied, message, err = newAliasClaims(w.client, w.log).
		reconcile(ctx, w.user.Organization, w.Name, w.canonicalIdForCleanup(), desired)
	if message != "" {
		w.log.Warn().Str("workspace", w.Name).Msg(message)
	}
	return applied, message, err
}

// emitWebProxyAliasWarning surfaces why the blueprint's alias was not applied
// as a warning status on the provisioning stream, which also records it on
// the provisioning job.
func (w *Workspace) emitWebProxyAliasWarning(opts *ProvisionOptions, message string) {
	if message == "" || opts == nil || opts.Messages == nil {
		return
	}
	opts.Messages <- models.WorkspaceStreamEvent{
		Type:       models.WorkspaceStreamEventTypeStatus,
		Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
		ObjectName: w.Name,
		Status:     models.WorkspaceStatusWarning,
		Message:    message,
	}
}

// applyWebProxyAliasToPod stamps the reconciled alias onto a pod manifest
// recovered from the Helm release, before the pod is re-created. A message
// replaces the release's; without one the release's message is kept.
func applyWebProxyAliasToPod(pod *corev1.Pod, alias, message string) {
	if alias != "" {
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels[helm.LabelWebProxyAlias] = alias
	} else {
		delete(pod.Labels, helm.LabelWebProxyAlias)
	}
	if message != "" {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[helm.AnnotationWebProxyAliasMessage] = message
	}
}

// releaseExists reports whether a Helm release named name exists in the
// client's target namespace, regardless of its status.
func releaseExists(helmClient *helm.Client, name string) (bool, error) {
	_, err := helmClient.GetRelease(name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
