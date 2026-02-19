package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/pkg/api/provisionerpb"
	"google.golang.org/grpc"
)

var ErrWorkspaceExists = errors.New("workspace already exists")
var ErrInvalidArgument = errors.New("invalid argument")

type Client struct {
	provisionerpb.ProvisionerServiceClient
	client *gapi.Client
}

func NewClient(cfg gapi.ClientConfig) (*Client, error) {
	gapiClient, err := gapi.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{
		ProvisionerServiceClient: provisionerpb.NewProvisionerServiceClient(gapiClient.Conn),
		client:                   gapiClient,
	}, nil
}

func (c *Client) Close() error {
	return c.client.Close()
}

// ProvisionHandshake reads the first stream event that needs to be handshake.
func (c *Client) ProvisionHandshake(ctx context.Context, userstr models.UserStr) (workspaceName string, jobID string, stream grpc.ServerStreamingClient[provisionerpb.ProvisionWorkspaceResponse], err error) {
	stream, err = c.ProvisionWorkspaceStream(ctx, &provisionerpb.ProvisionWorkspaceRequest{
		Userstr:      userstr.Raw,
		SendProgress: true,
		SendEvents:   true,
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to start provisioning stream: %w", err)
	}
	return c.waitForHandshakeMessage(stream)
}

// UpgradeHandshake reads the first stream event that needs to be handshake.
func (c *Client) UpgradeHandshake(ctx context.Context, workspace string, forceUpgrade bool) (workspaceName string, jobID string, stream grpc.ServerStreamingClient[provisionerpb.ProvisionWorkspaceResponse], err error) {
	stream, err = c.UpgradeWorkspaceStream(ctx, &provisionerpb.UpgradeWorkspaceRequest{
		Workspace:    workspace,
		SendProgress: true,
		SendEvents:   true,
		Force:        forceUpgrade,
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to start upgrade stream: %w", err)
	}
	return c.waitForHandshakeMessage(stream)
}

func (c *Client) waitForHandshakeMessage(
	stream grpc.ServerStreamingClient[provisionerpb.ProvisionWorkspaceResponse],
) (string, string, grpc.ServerStreamingClient[provisionerpb.ProvisionWorkspaceResponse], error) {
	first, err := stream.Recv()
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to receive first stream event (handshake expected): %w", err)
	}

	hs := first.GetHandshake()
	if hs == nil {
		return "", "", nil, fmt.Errorf("invalid first stream event: expected handshake, got %+v", first)
	}

	if hs.GetError() != "" {
		code, desc := extractErrorCodeAndDesc(hs.GetError())
		if code == "AlreadyExists" || code == "PreconditionFailed" {
			return "", "", nil, fmt.Errorf("%w: handshake failed: %s", ErrWorkspaceExists, desc)
		}
		if code == "InvalidArgument" {
			return "", "", nil, fmt.Errorf("%w: handshake failed: %s", ErrInvalidArgument, desc)
		}
		return "", "", nil, fmt.Errorf("handshake failed: %s", desc)
	}

	workspaceName := hs.GetWorkspace()
	jobID := hs.GetJobid()
	if workspaceName == "" {
		return "", "", nil, fmt.Errorf("handshake missing required field: workspace")
	}

	return workspaceName, jobID, stream, nil
}

func extractErrorCodeAndDesc(s string) (code string, desc string) {
	s = strings.TrimSpace(s)

	const descKey = "desc = "
	if i := strings.LastIndex(s, descKey); i >= 0 {
		desc = strings.TrimSpace(s[i+len(descKey):])
		desc = strings.Trim(desc, `"`)
	}

	const codeKey = "code = "
	if i := strings.LastIndex(s, codeKey); i >= 0 {
		rest := strings.TrimSpace(s[i+len(codeKey):])
		if j := strings.IndexAny(rest, " \t\r\n"); j >= 0 {
			code = strings.TrimSpace(rest[:j])
		} else {
			code = rest
		}
		code = strings.Trim(code, `"`)
	}

	return code, desc
}
