// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package config

import (
	"reflect"
	"testing"
)

func TestNormalizeNodeSelectorKeys(t *testing.T) {
	got, err := normalizeNodeSelectorKeys([]string{" k8shell.io/pool ", "zone", "k8shell.io/pool"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"k8shell.io/pool", "zone"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for _, bad := range []string{"", "has space", "a/b/c", "-x"} {
		if _, err := normalizeNodeSelectorKeys([]string{bad}); err == nil {
			t.Errorf("key %q should be rejected", bad)
		}
	}
}
