// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"errors"
	"fmt"
	"testing"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestK8sValidationMessage(t *testing.T) {
	t.Run("structured causes", func(t *testing.T) {
		err := k8serrors.NewInvalid(
			schema.GroupKind{Kind: "Pod"},
			"tomvit-81390bd",
			field.ErrorList{
				field.Invalid(
					field.NewPath("spec", "containers").Index(0).Child("resources", "requests"),
					"128Mi",
					"must be less than or equal to memory limit of 64Mi",
				),
			},
		)
		got := k8sValidationMessage(err)
		want := `spec.containers[0].resources.requests: Invalid value: "128Mi": must be less than or equal to memory limit of 64Mi`
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("wrapped invalid error stays classified and readable", func(t *testing.T) {
		inner := k8serrors.NewInvalid(
			schema.GroupKind{Kind: "Pod"},
			"ws",
			field.ErrorList{field.Forbidden(field.NewPath("spec"), "pod updates may not change fields other than ...")},
		)
		err := fmt.Errorf("failed to build resize patch: %w", inner)
		if !k8serrors.IsInvalid(err) {
			t.Fatalf("expected wrapped error to still be classified Invalid")
		}
		if got := k8sValidationMessage(err); got == "" {
			t.Fatalf("expected a non-empty message, got %q", got)
		}
	})

	t.Run("plain error falls back to its text", func(t *testing.T) {
		if got := k8sValidationMessage(errors.New("boom")); got != "boom" {
			t.Fatalf("got %q, want %q", got, "boom")
		}
	})
}
