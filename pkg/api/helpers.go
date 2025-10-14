package api

import (
	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/provisioner/pkg/api/provisionerpb"
)

type Client struct {
	provisionerpb.ProvisionerServiceClient
}

func NewClient(cfg gapi.ClientConfig) (*Client, error) {
	gapiClient, err := gapi.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{
		ProvisionerServiceClient: provisionerpb.NewProvisionerServiceClient(gapiClient.Conn),
	}, nil
}
