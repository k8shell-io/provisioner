// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"fmt"

	"github.com/k8shell-io/common/pkg/models"
	"k8s.io/apimachinery/pkg/api/resource"
)

const bytesPerMi = 1024 * 1024

// overcommitRequests derives container requests from the limits and the
// blueprint's resources.overcommit (request = limit / ratio). It returns the
// "cpu" and "memory" requests that apply; a resource without a ratio is left
// out so the chart keeps its default request for it. It returns nil when no
// ratio is set.
func overcommitRequests(res models.Resources) (map[string]interface{}, error) {
	oc := res.Overcommit
	if oc == nil {
		return nil, nil
	}

	out := map[string]interface{}{}

	if oc.CPU != nil {
		limit, err := resource.ParseQuantity(res.CPU)
		if err != nil {
			return nil, fmt.Errorf("invalid cpu limit %q: %w", res.CPU, err)
		}
		milli := int64(float64(limit.MilliValue()) / *oc.CPU)
		if milli < 1 {
			milli = 1
		}
		out["cpu"] = resource.NewMilliQuantity(milli, resource.DecimalSI).String()
	}

	if oc.Memory != nil {
		limit, err := resource.ParseQuantity(res.Memory)
		if err != nil {
			return nil, fmt.Errorf("invalid memory limit %q: %w", res.Memory, err)
		}
		mi := int64(float64(limit.Value())/ *oc.Memory) / bytesPerMi
		if mi < 1 {
			mi = 1
		}
		out["memory"] = resource.NewQuantity(mi*bytesPerMi, resource.BinarySI).String()
	}

	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
