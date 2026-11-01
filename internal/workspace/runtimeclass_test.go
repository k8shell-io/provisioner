// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"errors"
	"testing"

	"github.com/k8shell-io/common/pkg/models"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCheckResizable(t *testing.T) {
	kata := "kata-qemu"
	empty := ""
	tests := []struct {
		name    string
		rc      *string
		wantErr bool
	}{
		{"no runtime class", nil, false},
		{"empty runtime class", &empty, false},
		{"kata", &kata, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "ws-a"},
				Spec:       corev1.PodSpec{RuntimeClassName: tt.rc},
			}
			err := checkResizable(pod)
			if tt.wantErr != errors.Is(err, ErrResizeUnsupported) || (!tt.wantErr && err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSanitizeBlueprintForInjectionDropsKata(t *testing.T) {
	bp := &models.Blueprint{
		RuntimeClassName: "kata-qemu",
		Podman:           models.Podman{Enabled: true, Profile: "kata"},
	}
	got, err := sanitizeBlueprintForInjection(bp)
	if err != nil {
		t.Fatal(err)
	}
	if got.RuntimeClassName != "" || got.Podman.Enabled || got.Podman.Profile != "" {
		t.Fatalf("injected blueprint keeps kata settings: runtimeClass=%q podman=%+v", got.RuntimeClassName, got.Podman)
	}
	if bp.RuntimeClassName != "kata-qemu" || !bp.Podman.Enabled {
		t.Fatal("input blueprint was mutated")
	}
}
