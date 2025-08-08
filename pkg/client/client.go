package client

import (
	"bufio"
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

// Config represents the client configuration
type Config struct {
	BaseURL string `json:"baseURL"`
	APIKey  string `json:"apiKey"`
	Timeout int    `json:"timeout"`
}

// Client represents the provisioner API client
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// BlueprintListResponse represents the response for listing blueprints
type BlueprintListResponse map[string]BlueprintInfo

// BlueprintInfo represents blueprint information in the list
type BlueprintInfo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
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
	CustomBlueprint []byte
}

// TemplateOptions represents options for workspace templating
type TemplateOptions struct {
	Username        string
	Blueprint       string
	CustomBlueprint []byte
}

// // WorkspaceResponse represents a workspace response from the API
// type WorkspaceResponse struct {
// 	Name         string    `json:"name"`
// 	Username     string    `json:"username"`
// 	Blueprint    string    `json:"blueprint"`
// 	Deployed     time.Time `json:"deployed"`
// 	WorkspaceUrl string    `json:"workspaceUrl"`
// 	StatusUrl    string    `json:"statusUrl"`
// }

// // WorkspaceStatus represents the status of a workspace
// type WorkspaceStatus struct {
// 	Created       time.Time     `json:"created"`
// 	Status        string        `json:"status"`
// 	Message       string        `json:"message"`
// 	PodIP         string        `json:"podIP"`
// 	ProvisionTime time.Duration `json:"provisionTime"`
// }

// NewClient creates a new provisioner API client
func NewClient(config Config) *Client {
	if config.Timeout == 0 {
		config.Timeout = 30
	}

	return &Client{
		baseURL: strings.TrimSuffix(config.BaseURL, "/"),
		apiKey:  config.APIKey,
		httpClient: &http.Client{
			Timeout: time.Duration(config.Timeout) * time.Second,
		},
	}
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

// handleErrorResponse handles error responses from the API
func (c *Client) handleErrorResponse(resp *http.Response) error {
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("API error (status %d): failed to read error response: %w", resp.StatusCode, err)
	}

	var errResp ErrorResponse
	if err := json.Unmarshal(body, &errResp); err != nil {
		// If we can't parse the error response, return the raw body
		return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	return fmt.Errorf("API error (status %d): %s", resp.StatusCode, errResp.Error)
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
func (c *Client) ProvisionWorkspace(ctx context.Context, opts *ProvisionOptions) (*models.WorkspaceStatus, error) {
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

	var result *models.WorkspaceStatus
	err = c.handleResponse(resp, &result)
	return result, err
}

// ProvisionWorkspaceStream provisions a new workspace with streaming updates
func (c *Client) ProvisionWorkspaceStream(ctx context.Context, opts *ProvisionOptions, eventChan chan<- models.StreamEvent) error {
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

	// CHANGE: Use bufio.Scanner to read line by line for NDJSON
	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
		line := scanner.Text()

		// Skip empty lines
		if strings.TrimSpace(line) == "" {
			continue
		}

		// Parse each line as a separate JSON object
		var event models.StreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			// Log the error but continue processing other events
			fmt.Printf("Failed to unmarshal event: %v, line: %s\n", err, line)
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case eventChan <- event:
			// Event sent successfully
		}

		// Break on final events
		if event.Type == "status" {
			break
		}
	}

	// Check for scanner errors
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading stream: %w", err)
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

// GetWorkspaces retrieves a list of workspaces, optionally filtered by username and/or blueprint
func (c *Client) GetWorkspaces(ctx context.Context, username, blueprint string) ([]models.WorkspaceInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v1/workspaces", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Add query parameters
	q := req.URL.Query()
	if username != "" {
		q.Add("username", username)
	}
	if blueprint != "" {
		q.Add("blueprint", blueprint)
	}
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.handleErrorResponse(resp)
	}

	var workspaces []models.WorkspaceInfo
	if err := json.NewDecoder(resp.Body).Decode(&workspaces); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return workspaces, nil
}

// GetWorkspace retrieves details of a specific workspace by name
func (c *Client) GetWorkspace(ctx context.Context, name string) (*models.WorkspaceInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v1/workspaces/"+name, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.handleErrorResponse(resp)
	}

	var workspace models.WorkspaceInfo
	if err := json.NewDecoder(resp.Body).Decode(&workspace); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &workspace, nil
}

// GetWorkspaceStatus retrieves the current status of a workspace
func (c *Client) GetWorkspaceStatus(ctx context.Context, name string) (*models.WorkspaceStatus, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v1/workspaces/"+name+"/status", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.handleErrorResponse(resp)
	}

	var status models.WorkspaceStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &status, nil
}
