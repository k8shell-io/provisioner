// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"testing"

	"github.com/k8shell-io/common/pkg/models"
)

func ratio(v float64) *float64 { return &v }

func TestOvercommitRequests(t *testing.T) {
	tests := []struct {
		name string
		res  models.Resources
		want map[string]interface{}
	}{
		{
			name: "no overcommit keeps defaults",
			res:  models.Resources{CPU: "8", Memory: "8Gi"},
			want: nil,
		},
		{
			name: "empty overcommit keeps defaults",
			res:  models.Resources{CPU: "8", Memory: "8Gi", Overcommit: &models.Overcommit{}},
			want: nil,
		},
		{
			name: "memory only",
			res:  models.Resources{CPU: "8", Memory: "8Gi", Overcommit: &models.Overcommit{Memory: ratio(2)}},
			want: map[string]interface{}{"memory": "4Gi"},
		},
		{
			name: "cpu and memory separately",
			res:  models.Resources{CPU: "8", Memory: "8Gi", Overcommit: &models.Overcommit{CPU: ratio(4), Memory: ratio(2)}},
			want: map[string]interface{}{"cpu": "2", "memory": "4Gi"},
		},
		{
			name: "milli cpu",
			res:  models.Resources{CPU: "500m", Memory: "512Mi", Overcommit: &models.Overcommit{CPU: ratio(2)}},
			want: map[string]interface{}{"cpu": "250m"},
		},
		{
			name: "ratio of one equals the limit",
			res:  models.Resources{CPU: "2", Memory: "2Gi", Overcommit: &models.Overcommit{CPU: ratio(1), Memory: ratio(1)}},
			want: map[string]interface{}{"cpu": "2", "memory": "2Gi"},
		},
		{
			name: "memory rounded down to a whole Mi",
			res:  models.Resources{CPU: "1", Memory: "8Gi", Overcommit: &models.Overcommit{Memory: ratio(3)}},
			want: map[string]interface{}{"memory": "2730Mi"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := overcommitRequests(tt.res)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestOvercommitRequestsInvalidLimit(t *testing.T) {
	_, err := overcommitRequests(models.Resources{CPU: "8", Memory: "bogus", Overcommit: &models.Overcommit{Memory: ratio(2)}})
	if err == nil {
		t.Fatal("expected an error for an invalid memory limit")
	}
}
