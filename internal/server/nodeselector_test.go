// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"testing"

	"github.com/k8shell-io/provisioner/internal/nodepool"
)

func TestValidateNodeSelectors(t *testing.T) {
	opts := []nodepool.Key{{Key: "k8shell.io/pool", Values: []nodepool.Value{{Value: "gpu"}, {Value: "cpu"}}}}

	if err := validateNodeSelectors(nil, opts); err != nil {
		t.Errorf("empty selectors: %v", err)
	}
	if err := validateNodeSelectors(map[string][]string{"k8shell.io/pool": {"gpu", "cpu"}}, opts); err != nil {
		t.Errorf("valid selector: %v", err)
	}
	for _, bad := range []map[string][]string{
		{"k8shell.io/pool": {"tpu"}},
		{"k8shell.io/pool": {"gpu", "tpu"}},
		{"zone": {"a"}},
	} {
		if err := validateNodeSelectors(bad, opts); err == nil {
			t.Errorf("expected error for %v", bad)
		}
	}
	if err := validateNodeSelectors(map[string][]string{"k8shell.io/pool": {"gpu"}}, nil); err == nil {
		t.Error("expected error when no keys are configured")
	}
}
