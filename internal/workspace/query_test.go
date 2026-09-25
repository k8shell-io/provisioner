// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/k8shell-io/common/pkg/api/client/identity"
	commonv1 "github.com/k8shell-io/common/pkg/api/gen/go/common/v1"
	identityv1 "github.com/k8shell-io/common/pkg/api/gen/go/identity/v1"
	"google.golang.org/grpc"
)

// pagedUsersStub serves GetUsers from a fixed user list, paginated like the
// identity service: an unset limit falls back to defaultLimit and any limit
// is capped at maxLimit.
type pagedUsersStub struct {
	identityv1.IdentityServiceClient
	users        []string
	defaultLimit int
	maxLimit     int
}

func (s *pagedUsersStub) GetUsers(_ context.Context, req *identityv1.GetUsersRequest,
	_ ...grpc.CallOption) (*identityv1.UserList, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = s.defaultLimit
	}
	limit = min(limit, s.maxLimit)
	offset := min(int(req.GetOffset()), len(s.users))
	end := min(offset+limit, len(s.users))

	resp := &identityv1.UserList{}
	for _, u := range s.users[offset:end] {
		resp.Users = append(resp.Users, &commonv1.User{Username: u})
	}
	return resp, nil
}

func TestResolveObligationScopeRolesPaginates(t *testing.T) {
	users := make([]string, 0, 130)
	for i := range 129 {
		users = append(users, fmt.Sprintf("user%03d", i))
	}
	// Sorts last, so it's outside any first page.
	users = append(users, "vitvatom")

	tests := []struct {
		name     string
		maxLimit int
	}{
		{name: "server honours requested limit", maxLimit: 100},
		{name: "server caps below requested limit", maxLimit: 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &pagedUsersStub{users: users, defaultLimit: 50, maxLimit: tt.maxLimit}
			client := &identity.IdentityClient{IdentityServiceClient: stub}

			scope, err := resolveObligationScope(context.Background(), client,
				map[string]string{"roles": `["arc-admin","arc-user"]`})
			if err != nil {
				t.Fatalf("resolveObligationScope: %v", err)
			}
			if len(scope.usernames) != len(users) {
				t.Fatalf("got %d usernames, want %d", len(scope.usernames), len(users))
			}
			if !slices.Contains(scope.usernames, "vitvatom") {
				t.Fatalf("vitvatom missing from resolved usernames")
			}
		})
	}
}
