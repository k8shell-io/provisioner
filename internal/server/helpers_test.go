// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"fmt"
	"testing"

	ws "github.com/k8shell-io/provisioner/internal/workspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestConvertToGRPCErrorWebProxyAlias(t *testing.T) {
	cases := []struct {
		err  error
		want codes.Code
	}{
		{fmt.Errorf("claim: %w", ws.ErrWebProxyAliasHeld), codes.AlreadyExists},
		{fmt.Errorf("update: %w", ws.ErrNoWebProxyRoute), codes.FailedPrecondition},
	}
	for _, c := range cases {
		if got := status.Code(convertToGRPCError(c.err)); got != c.want {
			t.Errorf("convertToGRPCError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
