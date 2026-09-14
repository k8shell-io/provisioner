// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import "testing"

func TestApplyDNSIdentityDefaults(t *testing.T) {
	t.Run("both unset default to workspace name and organization", func(t *testing.T) {
		values := map[string]interface{}{}
		applyDNSIdentityDefaults(values, "Alice-WS1", "Acme Corp")

		if got := values["hostname"]; got != "alice-ws1" {
			t.Fatalf("hostname = %v, want alice-ws1", got)
		}
		if got := values["subdomain"]; got != "acme-corp" {
			t.Fatalf("subdomain = %v, want acme-corp", got)
		}
	})

	t.Run("blueprint values are preserved", func(t *testing.T) {
		values := map[string]interface{}{"hostname": "custom", "subdomain": "team"}
		applyDNSIdentityDefaults(values, "workspace1", "acme")

		if got := values["hostname"]; got != "custom" {
			t.Fatalf("hostname = %v, want custom", got)
		}
		if got := values["subdomain"]; got != "team" {
			t.Fatalf("subdomain = %v, want team", got)
		}
	})

	t.Run("only the missing key is defaulted", func(t *testing.T) {
		values := map[string]interface{}{"hostname": "custom"}
		applyDNSIdentityDefaults(values, "workspace1", "acme")

		if got := values["hostname"]; got != "custom" {
			t.Fatalf("hostname = %v, want custom", got)
		}
		if got := values["subdomain"]; got != "acme" {
			t.Fatalf("subdomain = %v, want acme", got)
		}
	})

	t.Run("empty organization leaves subdomain unset", func(t *testing.T) {
		values := map[string]interface{}{}
		applyDNSIdentityDefaults(values, "workspace1", "")

		if got := values["hostname"]; got != "workspace1" {
			t.Fatalf("hostname = %v, want workspace1", got)
		}
		if _, ok := values["subdomain"]; ok {
			t.Fatalf("subdomain = %v, want unset", values["subdomain"])
		}
	})
}
