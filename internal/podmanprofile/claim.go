// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package podmanprofile

import (
	"fmt"

	"github.com/k8shell-io/common/pkg/models"
	"k8s.io/apimachinery/pkg/api/resource"
)

// StorageSize returns the capacity of the storage in bytes: its sizeLimit for
// an ephemeral (emptyDir, memory) storage, the claim request otherwise.
func StorageSize(s models.Storage) (int64, bool) {
	if s.Type == "emptyDir" || s.Type == "memory" {
		if s.SizeLimit == "" {
			return 0, false
		}
		q, err := resource.ParseQuantity(s.SizeLimit)
		if err != nil {
			return 0, false
		}
		return q.Value(), true
	}
	return ClaimSize(s)
}

// IsPVC reports whether the storage is backed by a persistent volume claim.
func IsPVC(s models.Storage) bool {
	return s.Type != "emptyDir" && s.Type != "memory"
}

// ClaimSize returns the storage request of the storage's claim spec in bytes.
func ClaimSize(s models.Storage) (int64, bool) {
	resources, _ := s.ClaimSpec["resources"].(map[string]interface{})
	requests, _ := resources["requests"].(map[string]interface{})
	raw, ok := requests["storage"]
	if !ok {
		return 0, false
	}
	q, err := resource.ParseQuantity(fmt.Sprint(raw))
	if err != nil {
		return 0, false
	}
	return q.Value(), true
}

// ClaimVolumeMode returns the claim spec's volumeMode, Filesystem if unset.
func ClaimVolumeMode(s models.Storage) string {
	if mode, ok := s.ClaimSpec["volumeMode"].(string); ok && mode != "" {
		return mode
	}
	return "Filesystem"
}
