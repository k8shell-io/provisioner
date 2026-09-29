// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package blueprint

import (
	"testing"

	"github.com/k8shell-io/common/pkg/validator"
)

const celAliasBlueprint = "name: nats-course\ndescription: NATS course\nimage: myimage:latest\n" +
	"network:\n  webProxy:\n    port: 8080\n    alias: !cel \"user.username + '-nats'\"\n" +
	requiredBlueprintFields

// TestWebProxyAliasCEL checks that a !cel alias is evaluated per user, and
// that an evaluated alias is not validated at evaluation time: the
// provisioner validates it when it claims it, and provisions without it
// when it is invalid.
func TestWebProxyAliasCEL(t *testing.T) {
	bm := newTestManager(t, map[string]string{"nats-course.yaml": celAliasBlueprint})

	bp, err := bm.GetBlueprint("nats-course", TestScope())
	if err != nil {
		t.Fatalf("GetBlueprint: %v", err)
	}
	if bp.Network.WebProxy == nil || bp.Network.WebProxy.Alias != "testuser-nats" {
		t.Fatalf("alias = %+v, want testuser-nats", bp.Network.WebProxy)
	}

	scope := TestScope()
	scope.User.Username = "Bad.User"
	bp, err = bm.GetBlueprint("nats-course", scope)
	if err != nil {
		t.Fatalf("GetBlueprint with an invalid alias result: %v", err)
	}
	if got := bp.Network.WebProxy.Alias; got != "Bad.User-nats" || validator.IsWebProxyAlias(got) {
		t.Fatalf("alias = %q, want the unvalidated Bad.User-nats", got)
	}
}

func TestValidateRawBlueprintWebProxyAlias(t *testing.T) {
	bm := newTestManager(t, nil)
	base := "name: my-blueprint\ndescription: A test blueprint\nimage: myimage:latest\n" + requiredBlueprintFields

	cases := []struct {
		name      string
		alias     string
		wantIssue bool
	}{
		{"static valid", "nats-course", false},
		{"cel valid for the test user", "!cel \"user.username + '-nats'\"", false},
		{"double dash", "nats--course", true},
		{"uppercase", "Nats", true},
		{"dot", "nats.course", true},
		{"all digits (read as the port form)", "\"8080\"", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			yaml := base + "network:\n  webProxy:\n    port: 8080\n    alias: " + c.alias + "\n"
			issues, _, err := bm.ValidateRawBlueprint([]byte(yaml))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			found := false
			for _, issue := range issues {
				if issue.Field == "network.webProxy.alias" {
					found = true
				}
			}
			if found != c.wantIssue {
				t.Fatalf("alias issue found = %v, want %v (issues: %+v)", found, c.wantIssue, issues)
			}
		})
	}
}
