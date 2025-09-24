package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/k8shell-io/common/apiclient"
	"github.com/k8shell-io/common/models"
	provModels "github.com/k8shell-io/provisioner/pkg/models"
)

// Client represents the provisioner API client
type Client struct {
	*apiclient.Client
}

// BlueprintListResponse represents the response for listing blueprints
type BlueprintListResponse map[string]BlueprintInfo

// BlueprintInfo represents blueprint information in the list
type BlueprintInfo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ProvisionOptions represents options for workspace provisioning
type ProvisionOptions struct {
	UserStr models.UserStr
	Timeout int
	Stream  bool
}

// TemplateOptions represents options for workspace templating
type TemplateOptions struct {
	Username        string
	Blueprint       string
	CustomBlueprint []byte
}

func NewClient(config apiclient.Config) *Client {
	return &Client{
		Client: apiclient.NewClient(config, "provisioner-client"),
	}
}

// ListBlueprints lists all available blueprints
func (c *Client) ListBlueprints(ctx context.Context) (BlueprintListResponse, error) {
	resp, err := c.MakeRequest(ctx, "GET", "/api/v1/blueprints", nil, "")
	if err != nil {
		return nil, err
	}

	var result BlueprintListResponse
	_, err = c.HandleResponse(resp, &result)
	return result, err
}

// GetBlueprint gets a specific blueprint with user scope applied
func (c *Client) GetBlueprint(ctx context.Context, name, username string) (*models.Blueprint, error) {
	if name == "" {
		return nil, fmt.Errorf("blueprint name is required")
	}
	if username == "" {
		return nil, fmt.Errorf("username is required")
	}

	endpoint := fmt.Sprintf("/api/v1/blueprints/%s?username=%s",
		url.QueryEscape(name), url.QueryEscape(username))

	resp, err := c.MakeRequest(ctx, "GET", endpoint, nil, "")
	if err != nil {
		return nil, err
	}

	var result models.Blueprint
	_, err = c.HandleResponse(resp, &result)
	return &result, err
}

// GetRawBlueprint gets the raw blueprint configuration without scope processing
func (c *Client) GetRawBlueprint(ctx context.Context, name string) (map[string]interface{}, error) {
	if name == "" {
		return nil, fmt.Errorf("blueprint name is required")
	}

	endpoint := fmt.Sprintf("/api/v1/blueprints/%s/raw", url.QueryEscape(name))

	resp, err := c.MakeRequest(ctx, "GET", endpoint, nil, "")
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	_, err = c.HandleResponse(resp, &result)
	return result, err
}

// ComposeBlueprint composes a custom blueprint YAML with user scope
func (c *Client) ComposeBlueprint(ctx context.Context, username string, blueprintYAML []byte) (*models.Blueprint, error) {
	if username == "" {
		return nil, fmt.Errorf("username is required")
	}
	if len(blueprintYAML) == 0 {
		return nil, fmt.Errorf("blueprint YAML is required")
	}

	endpoint := fmt.Sprintf("/api/v1/blueprints/compose?username=%s", url.QueryEscape(username))

	resp, err := c.MakeRequest(ctx, "POST", endpoint, bytes.NewReader(blueprintYAML), "text/yaml")
	if err != nil {
		return nil, err
	}

	var result models.Blueprint
	_, err = c.HandleResponse(resp, &result)
	return &result, err
}

// TemplateWorkspace generates Kubernetes manifests for a workspace without provisioning
func (c *Client) TemplateWorkspace(ctx context.Context, opts *TemplateOptions) (string, error) {
	if opts == nil {
		return "", fmt.Errorf("template options are required")
	}
	if opts.Username == "" {
		return "", fmt.Errorf("username is required")
	}

	var endpoint string
	var body io.Reader
	var contentType string

	if len(opts.CustomBlueprint) > 0 {
		if opts.Blueprint != "" {
			return "", fmt.Errorf("cannot use both blueprint name and custom blueprint")
		}
		endpoint = fmt.Sprintf("/api/v1/workspaces/template?username=%s", url.QueryEscape(opts.Username))
		body = bytes.NewReader(opts.CustomBlueprint)
		contentType = "text/yaml"
	} else {
		if opts.Blueprint == "" {
			return "", fmt.Errorf("blueprint name is required when no custom blueprint is provided")
		}
		endpoint = fmt.Sprintf("/api/v1/workspaces/template?username=%s&blueprint=%s",
			url.QueryEscape(opts.Username), url.QueryEscape(opts.Blueprint))
	}

	resp, err := c.MakeRequest(ctx, "POST", endpoint, body, contentType)
	if err != nil {
		return "", err
	}

	content, err := c.HandleResponse(resp, nil)
	return content, err
}

// ProvisionWorkspace provisions a new workspace
func (c *Client) ProvisionWorkspace(ctx context.Context, opts *ProvisionOptions) (*provModels.WorkspaceStatus, error) {
	if opts == nil {
		return nil, fmt.Errorf("provision options are required")
	}
	if opts.Stream {
		return nil, fmt.Errorf("use ProvisionWorkspaceStream for streaming")
	}

	params := url.Values{}
	params.Set("userstr", opts.UserStr.Raw)

	if opts.Timeout > 0 {
		params.Set("timeout", strconv.Itoa(opts.Timeout))
	}

	resp, err := c.MakeRequest(ctx, "POST", fmt.Sprintf("/api/v1/workspaces?%s", params.Encode()), nil, "")
	if err != nil {
		return nil, err
	}

	var result *provModels.WorkspaceStatus
	_, err = c.HandleResponse(resp, &result)
	return result, err
}

// ProvisionWorkspaceStream provisions a new workspace with streaming updates
func (c *Client) ProvisionWorkspaceStream(ctx context.Context, opts *ProvisionOptions,
	eventChan chan<- provModels.StreamEvent) error {
	if opts == nil {
		return fmt.Errorf("provision options are required")
	}
	if eventChan == nil {
		return fmt.Errorf("event channel is required")
	}

	params := url.Values{}
	params.Set("userstr", opts.UserStr.Raw)
	params.Set("stream", "true")

	if opts.Timeout > 0 {
		params.Set("timeout", strconv.Itoa(opts.Timeout))
	}

	resp, err := c.MakeRequest(ctx, "POST", fmt.Sprintf("/api/v1/workspaces?%s", params.Encode()), nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	err = c.CheckError(resp)
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
		line := scanner.Text()

		if strings.TrimSpace(line) == "" {
			continue
		}

		var event provModels.StreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			fmt.Printf("Failed to unmarshal event: %v, line: %s\n", err, line)
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case eventChan <- event:
		}

		if event.Type == "status" && event.Status != "Starting" {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading stream: %w", err)
	}

	return nil
}

// GetWorkspaces retrieves a list of workspaces, optionally filtered by username and/or blueprint
func (c *Client) GetWorkspaces(ctx context.Context, username, blueprint string) ([]provModels.WorkspaceInfo, error) {
	params := url.Values{}
	if username != "" {
		params.Set("username", username)
	}
	if blueprint != "" {
		params.Set("blueprint", blueprint)
	}

	endpoint := "/api/v1/workspaces"
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}

	resp, err := c.MakeRequest(ctx, "GET", endpoint, nil, "")
	if err != nil {
		return nil, err
	}

	var workspaces []provModels.WorkspaceInfo
	_, err = c.HandleResponse(resp, &workspaces)
	return workspaces, err
}

// GetWorkspace retrieves details of a specific workspace by name
func (c *Client) GetWorkspace(ctx context.Context, name string) (*provModels.WorkspaceInfo, error) {
	endpoint := fmt.Sprintf("/api/v1/workspaces/%s", url.QueryEscape(name))

	resp, err := c.MakeRequest(ctx, "GET", endpoint, nil, "")
	if err != nil {
		return nil, err
	}

	var workspace provModels.WorkspaceInfo
	_, err = c.HandleResponse(resp, &workspace)
	if err != nil {
		if apiErr, ok := err.(*apiclient.APIError); ok && apiErr.StatusCode == 404 {
			return nil, fmt.Errorf("%w: %s", provModels.ErrWorkspaceNotFound, name)
		}
		return nil, err
	}

	return &workspace, nil
}

// GetWorkspaceStatus retrieves the current status of a workspace
func (c *Client) GetWorkspaceStatus(ctx context.Context, name string) (*provModels.WorkspaceStatus, error) {
	endpoint := fmt.Sprintf("/api/v1/workspaces/%s/status", url.QueryEscape(name))

	resp, err := c.MakeRequest(ctx, "GET", endpoint, nil, "")
	if err != nil {
		return nil, err
	}

	var status provModels.WorkspaceStatus
	_, err = c.HandleResponse(resp, &status)
	if err != nil {
		if apiErr, ok := err.(*apiclient.APIError); ok && apiErr.StatusCode == 404 {
			return nil, fmt.Errorf("%w: %s", provModels.ErrWorkspaceNotFound, name)
		}
		return nil, err
	}

	return &status, nil
}

// DeleteWorkspace deletes a specific workspace by name
func (c *Client) DeleteWorkspace(ctx context.Context, name string) error {
	endpoint := fmt.Sprintf("/api/v1/workspaces/%s", url.QueryEscape(name))

	resp, err := c.MakeRequest(ctx, "DELETE", endpoint, nil, "")
	if err != nil {
		if apiErr, ok := err.(*apiclient.APIError); ok && apiErr.StatusCode == 404 {
			return fmt.Errorf("%w: %s", provModels.ErrWorkspaceNotFound, name)
		}
		return err
	}

	defer resp.Body.Close()
	return nil
}
