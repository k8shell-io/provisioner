package api

import (
	"context"
	"fmt"

	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/common/pkg/models"
	"github.com/k8shell-io/provisioner/pkg/api/provisionerpb"
	"google.golang.org/grpc"
)

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

// HandshakeInfo reads the FIRST stream event that needs to be handshake.
func (c *Client) Handshake(ctx context.Context, userstr models.UserStr) (workspaceName string, jobID string, stream grpc.ServerStreamingClient[provisionerpb.ProvisionWorkspaceResponse], err error) {
	stream, err = c.ProvisionWorkspaceStream(ctx, &provisionerpb.ProvisionWorkspaceRequest{
		Userstr:      userstr.Raw,
		SendProgress: true,
		SendEvents:   true,
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to start provisioning stream: %w", err)
	}

	first, err := stream.Recv()
	if err != nil {
		return "", "", nil, fmt.Errorf("failed to receive first stream event (handshake expected): %w", err)
	}

	hs := first.GetHandshake()
	if hs == nil {
		return "", "", nil, fmt.Errorf("invalid first stream event: expected handshake, got %+v", first)
	}

	if hs.GetError() != "" {
		return "", "", nil, fmt.Errorf("handshake failed: %s", hs.GetError())
	}

	workspaceName = hs.GetWorkspace()
	jobID = hs.GetJobid()
	if workspaceName == "" {
		return "", "", nil, fmt.Errorf("handshake missing required field: workspace")
	}

	return workspaceName, jobID, stream, nil
}
