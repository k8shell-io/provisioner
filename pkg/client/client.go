package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/k8shell-io/provisioner/pkg/models"
)

// Client represents the provisioner API client
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// Config represents the client configuration
type Config struct {
	BaseURL    string        `json:"baseURL"`
	APIKey     string        `json:"apiKey"`
	Timeout    time.Duration `json:"timeout"`
	UserAgent  string        `json:"userAgent"`
	HTTPClient *http.Client  `json:"-"`
}

// BlueprintListResponse represents the response for listing blueprints
type BlueprintListResponse map[string]BlueprintInfo

// BlueprintInfo represents blueprint information in the list
type BlueprintInfo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// WorkspaceStatusResponse represents the response for workspace operations
type WorkspaceStatusResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	PodIP   string `json:"podIP"`
}

// StreamEvent represents a streaming event
type StreamEvent struct {
	Type       string `json:"type"`
	Timestamp  string `json:"timestamp,omitempty"`
	ObjectName string `json:"objectName,omitempty"`
	Message    string `json:"message,omitempty"`
	Status     string `json:"status,omitempty"`
	PodIP      string `json:"podIP,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ErrorResponse represents an API error response
type ErrorResponse struct {
	Error string `json:"error"`
}

// ProvisionOptions represents options for workspace provisioning
type ProvisionOptions struct {
	Username        string
	Blueprint       string
	Timeout         int
	Stream          bool
	CustomBlueprint []byte // YAML content for custom blueprints
}

// TemplateOptions represents options for workspace templating
type TemplateOptions struct {
	Username        string
	Blueprint       string
	CustomBlueprint []byte // YAML content for custom blueprints
}

// NewClient creates a new provisioner API client
func NewClient(config *Config) (*Client, error) {
	if config.BaseURL == "" {
		return nil, fmt.Errorf("baseURL is required")
	}

	if config.APIKey == "" {
		return nil, fmt.Errorf("apiKey is required")
	}

	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: config.Timeout,
		}
	}

	return &Client{
		baseURL:    strings.TrimSuffix(config.BaseURL, "/"),
		apiKey:     config.APIKey,
		httpClient: httpClient,
	}, nil
}

// makeRequest makes an HTTP request to the API
func (c *Client) makeRequest(ctx context.Context, method, endpoint string, body io.Reader, contentType string) (*http.Response, error) {
	url := c.baseURL + endpoint

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", "k8shell-provisioner-client/1.0")

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	if method == "GET" || method == "DELETE" {
		req.Header.Set("Accept", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	return resp, nil
}

// handleResponse handles API response and error parsing
func (c *Client) handleResponse(resp *http.Response, v interface{}) error {
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		var errResp ErrorResponse
		if err := json.Unmarshal(body, &errResp); err != nil {
			return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
		}
		return fmt.Errorf("API error (status %d): %s", resp.StatusCode, errResp.Error)
	}

	if v != nil {
		if err := json.Unmarshal(body, v); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}

// ListBlueprints lists all available blueprints
func (c *Client) ListBlueprints(ctx context.Context) (BlueprintListResponse, error) {
	resp, err := c.makeRequest(ctx, "GET", "/api/v1/blueprints", nil, "")
	if err != nil {
		return nil, err
	}

	var result BlueprintListResponse
	err = c.handleResponse(resp, &result)
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

	resp, err := c.makeRequest(ctx, "GET", endpoint, nil, "")
	if err != nil {
		return nil, err
	}

	var result models.Blueprint
	err = c.handleResponse(resp, &result)
	return &result, err
}

// GetRawBlueprint gets the raw blueprint configuration without scope processing
func (c *Client) GetRawBlueprint(ctx context.Context, name string) (map[string]interface{}, error) {
	if name == "" {
		return nil, fmt.Errorf("blueprint name is required")
	}

	endpoint := fmt.Sprintf("/api/v1/blueprints/%s/raw", url.QueryEscape(name))

	resp, err := c.makeRequest(ctx, "GET", endpoint, nil, "")
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	err = c.handleResponse(resp, &result)
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

	resp, err := c.makeRequest(ctx, "POST", endpoint, bytes.NewReader(blueprintYAML), "text/yaml")
	if err != nil {
		return nil, err
	}

	var result models.Blueprint
	err = c.handleResponse(resp, &result)
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
		// Custom blueprint approach
		if opts.Blueprint != "" {
			return "", fmt.Errorf("cannot use both blueprint name and custom blueprint")
		}
		endpoint = fmt.Sprintf("/api/v1/workspaces/template?username=%s", url.QueryEscape(opts.Username))
		body = bytes.NewReader(opts.CustomBlueprint)
		contentType = "text/yaml"
	} else {
		// Blueprint name approach
		if opts.Blueprint == "" {
			return "", fmt.Errorf("blueprint name is required when no custom blueprint is provided")
		}
		endpoint = fmt.Sprintf("/api/v1/workspaces/template?username=%s&blueprint=%s",
			url.QueryEscape(opts.Username), url.QueryEscape(opts.Blueprint))
	}

	resp, err := c.makeRequest(ctx, "POST", endpoint, body, contentType)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		var errResp ErrorResponse
		if err := json.Unmarshal(bodyBytes, &errResp); err != nil {
			return "", fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(bodyBytes))
		}
		return "", fmt.Errorf("API error (status %d): %s", resp.StatusCode, errResp.Error)
	}

	yamlBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	return string(yamlBytes), nil
}

// ProvisionWorkspace provisions a new workspace
func (c *Client) ProvisionWorkspace(ctx context.Context, opts *ProvisionOptions) (*WorkspaceStatusResponse, error) {
	if opts == nil {
		return nil, fmt.Errorf("provision options are required")
	}
	if opts.Username == "" {
		return nil, fmt.Errorf("username is required")
	}
	if opts.Stream {
		return nil, fmt.Errorf("use ProvisionWorkspaceStream for streaming")
	}

	params := url.Values{}
	params.Set("username", opts.Username)

	if opts.Timeout > 0 {
		params.Set("timeout", strconv.Itoa(opts.Timeout))
	}

	var endpoint string
	var body io.Reader
	var contentType string

	if len(opts.CustomBlueprint) > 0 {
		// Custom blueprint approach
		if opts.Blueprint != "" {
			return nil, fmt.Errorf("cannot use both blueprint name and custom blueprint")
		}
		endpoint = fmt.Sprintf("/api/v1/workspaces?%s", params.Encode())
		body = bytes.NewReader(opts.CustomBlueprint)
		contentType = "text/yaml"
	} else {
		// Blueprint name approach
		if opts.Blueprint == "" {
			return nil, fmt.Errorf("blueprint name is required when no custom blueprint is provided")
		}
		params.Set("blueprint", opts.Blueprint)
		endpoint = fmt.Sprintf("/api/v1/workspaces?%s", params.Encode())
	}

	resp, err := c.makeRequest(ctx, "POST", endpoint, body, contentType)
	if err != nil {
		return nil, err
	}

	var result WorkspaceStatusResponse
	err = c.handleResponse(resp, &result)
	return &result, err
}

// ProvisionWorkspaceStream provisions a new workspace with streaming updates
func (c *Client) ProvisionWorkspaceStream(ctx context.Context, opts *ProvisionOptions, eventChan chan<- StreamEvent) error {
	if opts == nil {
		return fmt.Errorf("provision options are required")
	}
	if opts.Username == "" {
		return fmt.Errorf("username is required")
	}
	if eventChan == nil {
		return fmt.Errorf("event channel is required")
	}

	defer close(eventChan)

	params := url.Values{}
	params.Set("username", opts.Username)
	params.Set("stream", "true")

	if opts.Timeout > 0 {
		params.Set("timeout", strconv.Itoa(opts.Timeout))
	}

	var endpoint string
	var body io.Reader
	var contentType string

	if len(opts.CustomBlueprint) > 0 {
		if opts.Blueprint != "" {
			return fmt.Errorf("cannot use both blueprint name and custom blueprint")
		}
		endpoint = fmt.Sprintf("/api/v1/workspaces?%s", params.Encode())
		body = bytes.NewReader(opts.CustomBlueprint)
		contentType = "text/yaml"
	} else {
		if opts.Blueprint == "" {
			return fmt.Errorf("blueprint name is required when no custom blueprint is provided")
		}
		params.Set("blueprint", opts.Blueprint)
		endpoint = fmt.Sprintf("/api/v1/workspaces?%s", params.Encode())
	}

	resp, err := c.makeRequest(ctx, "POST", endpoint, body, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		var errResp ErrorResponse
		if err := json.Unmarshal(bodyBytes, &errResp); err != nil {
			return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(bodyBytes))
		}
		return fmt.Errorf("API error (status %d): %s", resp.StatusCode, errResp.Error)
	}

	decoder := json.NewDecoder(resp.Body)
	for {
		var event StreamEvent
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("failed to decode streaming event: %w", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case eventChan <- event:
			// Event sent successfully
		}

		// Break on final events
		if event.Type == "status" || event.Type == "error" {
			break
		}
	}

	return nil
}

// Ping checks if the API server is reachable
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.makeRequest(ctx, "GET", "/api/v1/blueprints", nil, "")
	if err != nil {
		return fmt.Errorf("ping failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("ping failed with status: %d", resp.StatusCode)
	}

	return nil
}

// SetTimeout sets the HTTP client timeout
func (c *Client) SetTimeout(timeout time.Duration) {
	c.httpClient.Timeout = timeout
}

// GetBaseURL returns the base URL of the client
func (c *Client) GetBaseURL() string {
	return c.baseURL
}
