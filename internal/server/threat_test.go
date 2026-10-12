// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"

	authzv1 "github.com/k8shell-io/common/pkg/api/gen/go/authz/v1"
	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/common/pkg/models"
	natsc "github.com/k8shell-io/common/pkg/nats"
)

type fakeAuthz struct {
	authzv1.AuthzServiceClient
	req  *authzv1.EvaluateRequest
	resp *authzv1.EvaluateResponse
	err  error
}

func (f *fakeAuthz) Evaluate(_ context.Context, req *authzv1.EvaluateRequest, _ ...grpc.CallOption) (*authzv1.EvaluateResponse, error) {
	f.req = req
	return f.resp, f.err
}

func TestApplyDetectPolicy(t *testing.T) {
	now := time.Unix(1791700000, 0)
	override := &natsc.ThreatDetectionOverride{Detect: "all", SetBy: "admin", SetAt: 1}

	t.Run("detect obligation is stored with the subject", func(t *testing.T) {
		ob, found := authz.ParseDetectObligation(map[string]string{"detect": "container-escape"})
		d := &natsc.ThreatDetection{}
		applyDetectPolicy(ob, found, "alice", now)(d)
		if d.Policy == nil || d.Policy.Detect != "container-escape" ||
			d.Policy.EvaluatedFor != "alice" || d.Policy.EvaluatedAt != now.Unix() {
			t.Fatalf("unexpected policy: %+v", d.Policy)
		}
	})

	t.Run("no detect obligation clears the policy and keeps the override", func(t *testing.T) {
		ob, found := authz.ParseDetectObligation(nil)
		d := &natsc.ThreatDetection{
			Policy:   &natsc.ThreatDetectionPolicy{Detect: "all"},
			Override: override,
		}
		applyDetectPolicy(ob, found, "alice", now)(d)
		if d.Policy != nil {
			t.Fatalf("policy not cleared: %+v", d.Policy)
		}
		if d.Override != override {
			t.Fatalf("override changed: %+v", d.Override)
		}
	})

	t.Run("re-provisioning replaces the policy and keeps the override", func(t *testing.T) {
		ob, found := authz.ParseDetectObligation(map[string]string{"detect": "none"})
		d := &natsc.ThreatDetection{
			Policy:   &natsc.ThreatDetectionPolicy{Detect: "all", EvaluatedFor: "bob"},
			Override: override,
		}
		applyDetectPolicy(ob, found, "alice", now)(d)
		if d.Policy == nil || d.Policy.Detect != "none" || d.Policy.EvaluatedFor != "alice" {
			t.Fatalf("unexpected policy: %+v", d.Policy)
		}
		if d.Override != override {
			t.Fatalf("override changed: %+v", d.Override)
		}
	})
}

func TestEvaluateThreatDetect(t *testing.T) {
	t.Run("standalone", func(t *testing.T) {
		f := &fakeAuthz{resp: &authzv1.EvaluateResponse{Obligations: map[string]string{"detect": "all"}}}
		res, err := evaluateThreatDetect(context.Background(), f, "tok", threatDetectParams{
			workspace: "ws-1", owner: "alice", blueprint: &models.Blueprint{Name: "bp"},
			mode: authz.WorkspaceProvisionModeStandalone, namespace: "k8shell",
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Obligations["detect"] != "all" {
			t.Fatalf("obligations: %v", res.Obligations)
		}
		if f.req.Action != "threat:detect" || f.req.Package != "threat" || f.req.Token != "tok" ||
			f.req.Resource.Id != "ws-1" || f.req.Resource.Attributes["owner"] != "alice" ||
			f.req.Context["mode"] != "standalone" || f.req.Context["namespace"] != "k8shell" {
			t.Fatalf("unexpected request: %+v", f.req)
		}
		if f.req.Resource.Attributes["blueprint"] != "bp" || f.req.Context["blueprint"] == "" {
			t.Fatalf("blueprint not sent: attrs=%v context=%v", f.req.Resource.Attributes, f.req.Context)
		}
	})

	t.Run("inject uses the workload namespace and identity", func(t *testing.T) {
		f := &fakeAuthz{resp: &authzv1.EvaluateResponse{}}
		_, err := evaluateThreatDetect(context.Background(), f, "tok", threatDetectParams{
			workspace: "ws-1", owner: "alice", mode: authz.WorkspaceProvisionModeInject,
			namespace: "apps", workloadName: "web", workloadKind: "Deployment",
		})
		if err != nil {
			t.Fatal(err)
		}
		c := f.req.Context
		if c["mode"] != "inject" || c["namespace"] != "apps" ||
			c["workload_name"] != "web" || c["workload_kind"] != "Deployment" {
			t.Fatalf("unexpected context: %v", c)
		}
		if _, ok := c["blueprint"]; ok {
			t.Fatalf("blueprint sent without one: %v", c)
		}
	})

	t.Run("authz error is returned", func(t *testing.T) {
		f := &fakeAuthz{err: errors.New("boom")}
		if _, err := evaluateThreatDetect(context.Background(), f, "tok", threatDetectParams{
			workspace: "ws-1", owner: "alice", mode: authz.WorkspaceProvisionModeStandalone, namespace: "k8shell",
		}); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("invalid request is rejected before calling authz", func(t *testing.T) {
		f := &fakeAuthz{}
		if _, err := evaluateThreatDetect(context.Background(), f, "tok", threatDetectParams{workspace: "ws-1"}); err == nil {
			t.Fatal("expected error")
		}
		if f.req != nil {
			t.Fatal("authz must not be called")
		}
	})
}
