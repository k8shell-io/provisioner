// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"

	commonv1 "github.com/k8shell-io/common/pkg/api/gen/go/common/v1"
)

// serviceDescription is the short human-readable summary of what the
// provisioner does, returned by GetVersionInfo.
const serviceDescription = "Creates and tears down workspace pods per blueprint."

// GetVersionInfo returns the provisioner's released version, the git commit it
// was built from, and a short description of the service. The version and
// commit are injected at build time via -ldflags into main.PROVISIONER_VERSION
// / main.PROVISIONER_COMMIT (see docker/provisioner/Dockerfile) and threaded
// through NewServer; local builds report the development placeholders.
func (p *ProvisionerService) GetVersionInfo(_ context.Context,
	_ *commonv1.GetVersionInfoRequest) (*commonv1.GetVersionInfoResponse, error) {
	return &commonv1.GetVersionInfoResponse{
		Version:     p.server.appVersion,
		CommitId:    p.server.commit,
		Description: serviceDescription,
	}, nil
}
