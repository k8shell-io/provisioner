// Copyright 2025 the k8Shell authors

package server

// @title        K8shell Provisioner API
// @version      1.0
// @description  This is the API documentation for the K8shell provisioner service.
//
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	identity "github.com/k8shell-io/identity/pkg/client"
	_ "github.com/k8shell-io/provisioner/docs"
	"github.com/k8shell-io/provisioner/internal/blueprint"
	"github.com/k8shell-io/provisioner/internal/log"
	"github.com/k8shell-io/provisioner/internal/workspace"
	"github.com/k8shell-io/provisioner/pkg/models"
	"github.com/rs/zerolog"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

// RESTApiService represents the REST API service for the K8Shell Provisioner server.
type RESTApiService struct {
	server *Server
	log    *zerolog.Logger
	engine *gin.Engine
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error string `json:"error" example:"Error message"`
}

// BlueprintListResponse represents the response for listing blueprints
type BlueprintListResponse map[string]BlueprintInfo

// BlueprintInfo represents blueprint information in the list
type BlueprintInfo struct {
	Name string `json:"name" example:"dev"`
	URL  string `json:"url" example:"/api/v1/blueprints/dev"`
}

// // WorkspaceResponse represents a response containing a list of workspaces
// type WorkspaceResponse struct {
// 	models.WorkspaceInfo
// 	WorkspaceUrl string `json:"workspaceUrl" example:"/api/v1/workspaces/dev-user123"`
// 	StatusUrl    string `json:"statusUrl" example:"/api/v1/workspaces/dev-user123/status"`
// }

// NewRESTAPI creates a new REST API service
func NewRESTAPI(server *Server) (*RESTApiService, error) {
	log := log.NewLogger("api")

	gin.SetMode(gin.ReleaseMode)

	engine := gin.New()
	engine.Use(gin.Recovery())

	return &RESTApiService{
		server: server,
		log:    log,
		engine: engine,
	}, nil
}

// apiKeyMiddleware checks for the presence of a valid API key in the request header
func (a *RESTApiService) apiKeyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		const prefix = "Bearer "

		if !strings.HasPrefix(authHeader, prefix) {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Unauthorized: missing or malformed Authorization header",
			})
			c.Abort()
			return
		}

		providedKey := strings.TrimPrefix(authHeader, prefix)
		expectedKey := a.server.config.Http.APIKey

		if providedKey != expectedKey {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Unauthorized: invalid API key",
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// loggingMiddleware logs requests and responses
func (a *RESTApiService) loggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		method := c.Request.Method

		a.log.Debug().Msgf("Request: method %s, path %s", method, path)

		c.Next()

		latency := time.Since(start)
		statusCode := c.Writer.Status()

		if statusCode >= 400 {
			a.log.Error().Msgf("Response: status %d, method %s, path %s, latency %s",
				statusCode, method, path, latency)
		} else {
			a.log.Debug().Msgf("Response: status %d, method %s, path %s, latency %s",
				statusCode, method, path, latency)
		}
	}
}

// initializeRouter sets up all routes
func (a *RESTApiService) initializeRouter() {
	a.engine.Use(a.loggingMiddleware())

	// API routes with authentication
	api := a.engine.Group("/api/v1")
	api.Use(a.apiKeyMiddleware())
	{
		// Blueprint routes
		blueprints := api.Group("/blueprints")
		{
			blueprints.GET("", a.GetBlueprints)
			blueprints.GET("/:name", a.GetBlueprint)
			blueprints.GET("/:name/raw", a.GetRawBlueprint)
			blueprints.POST("/compose", a.ComposeBlueprint)
		}

		// Workspace routes
		workspaces := api.Group("/workspaces")
		{
			workspaces.GET("", a.GetWorkspaces)
			workspaces.GET("/:name", a.GetWorkspace)
			workspaces.GET("/:name/status", a.GetWorkspaceStatus)
			workspaces.POST("/template", a.TemplateWorkspace)
			workspaces.POST("", a.ProvisionWorkspace)
		}
	}

	// Swagger documentation (no auth required)
	a.engine.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// 404 handler
	a.engine.NoRoute(func(c *gin.Context) {
		a.log.Debug().Msgf("404 Not Found: %s %s", c.Request.Method, c.Request.URL.Path)
		c.JSON(http.StatusNotFound, gin.H{
			"error": "404 route not found",
		})
	})

	a.logRoutes()
}

// logRoutes logs all registered routes
func (a *RESTApiService) logRoutes() {
	routes := a.engine.Routes()
	for _, route := range routes {
		a.log.Debug().Msgf("Route: %s %s", route.Method, route.Path)
	}
}

// Serve starts the HTTP server
func (a *RESTApiService) Serve(ctx context.Context) {
	a.initializeRouter()

	server := &http.Server{
		Handler: a.engine,
		Addr:    fmt.Sprintf(":%d", a.server.config.Http.Port),
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		<-ctx.Done()
		a.log.Info().Msg("Shutting down REST API server...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			a.log.Error().Err(err).Msg("REST API server shutdown failed")
		} else {
			a.log.Info().Msg("REST API server shutdown complete")
		}
		close(idleConnsClosed)
	}()

	a.log.Info().Msgf("Starting API server on %s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		a.log.Error().Err(err).Msg("Failed to start API server")
	}

	<-idleConnsClosed
}

// *** Blueprints

// GetBlueprints handles the GET request for blueprints
// @Summary      List available blueprints
// @Description  Get a list of all available blueprint names
// @Tags         blueprints
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  BlueprintListResponse  "List of available blueprints"
// @Failure      404  {object}  ErrorResponse          "No blueprints found"
// @Failure      401  {object}  ErrorResponse          "Unauthorized"
// @Router       /api/v1/blueprints [get]
func (a *RESTApiService) GetBlueprints(c *gin.Context) {
	blueprints := a.server.bpManager.ListBlueprintNames()
	if len(blueprints) == 0 {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "No blueprints found",
		})
		return
	}

	response := gin.H{}
	for _, bp := range blueprints {
		response[bp] = gin.H{
			"name": bp,
			"url":  fmt.Sprintf("/api/v1/blueprints/%s", bp),
		}
	}

	c.JSON(http.StatusOK, response)
}

// GetBlueprint handles the GET request for a specific blueprint
// @Summary      Get a specific blueprint
// @Description  Get a blueprint by name with user scope applied
// @Tags         blueprints
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        name      path    string  true   "Blueprint name"
// @Param        username  query   string  true   "Username for scope resolution"
// @Success      200       {object}  models.Blueprint  "Blueprint details"
// @Failure      400       {object}  ErrorResponse     "Bad request - missing username"
// @Failure      404       {object}  ErrorResponse     "Blueprint not found"
// @Failure      401       {object}  ErrorResponse     "Unauthorized"
// @Failure      500       {object}  ErrorResponse     "Internal server error"
// @Router       /api/v1/blueprints/{name} [get]
func (a *RESTApiService) GetBlueprint(c *gin.Context) {
	name := c.Param("name")
	username := c.Query("username")

	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Username is required",
		})
		return
	}

	// Get the user's blueprint scope
	scope, errx := a.server.GetBlueprintScope(c.Request.Context(), username, "", "")
	if errx != nil {
		var eresp identity.ErrorResponse
		if errors.As(errx, &eresp) {
			c.JSON(eresp.Status, gin.H{
				"error": eresp.Msg,
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get user: %v", errx),
		})
		return
	}

	blueprint, err := a.server.bpManager.GetBlueprint(name, scope)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Blueprint not found: %s", name),
		})
		return
	}

	c.JSON(http.StatusOK, blueprint)
}

// GetRawBlueprint handles the GET request for a specific raw blueprint
// @Summary      Get raw blueprint
// @Description  Get the raw blueprint configuration without scope processing
// @Tags         blueprints
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        name  path    string  true  "Blueprint name"
// @Success      200   {object}  object  "Raw blueprint configuration"
// @Failure      404   {object}  ErrorResponse        	 "Blueprint not found"
// @Failure      401   {object}  ErrorResponse        	 "Unauthorized"
// @Router       /api/v1/blueprints/{name}/raw [get]
func (a *RESTApiService) GetRawBlueprint(c *gin.Context) {
	name := c.Param("name")

	rawBp, err := a.server.bpManager.GetRawBlueprint(name)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Raw blueprint not found: %s", name),
		})
		return
	}

	c.JSON(http.StatusOK, rawBp)
}

// ComposeBlueprint handles the POST request to compose a blueprint
// @Summary      Compose a custom blueprint
// @Description  Compose a custom blueprint YAML with user scope
// @Tags         blueprints
// @Accept       text/yaml,application/x-yaml
// @Produce      json
// @Security     BearerAuth
// @Param        username  query   string  true   "Username for scope resolution"
// @Param        blueprint body    string  true   "Custom blueprint YAML"
// @Success      200       {object}  models.Blueprint  "Composed blueprint"
// @Failure      400       {object}  ErrorResponse     "Bad request - validation failed"
// @Failure      415       {object}  ErrorResponse     "Unsupported media type"
// @Failure      401       {object}  ErrorResponse     "Unauthorized"
// @Failure      500       {object}  ErrorResponse     "Internal server error"
// @Router       /api/v1/blueprints/compose [post]
func (a *RESTApiService) ComposeBlueprint(c *gin.Context) {
	contentType := c.GetHeader("Content-Type")
	username := c.Query("username")

	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Username is required",
		})
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Failed to read request body",
		})
		return
	}

	var blueprintYAML []byte
	if strings.Contains(contentType, "text/yaml") || strings.Contains(contentType, "application/x-yaml") {
		blueprintYAML = body
	} else {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{
			"error": "Unsupported content type, expected text/yaml or application/x-yaml",
		})
		return
	}

	// Validate the custom blueprint YAML
	validationErrors := models.ValidateCustomBlueprint(blueprintYAML)
	if len(validationErrors) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Blueprint validation failed: %s", strings.Join(validationErrors, "; ")),
		})
		return
	}

	// Get the user's blueprint scope
	scope, errx := a.server.GetBlueprintScope(c.Request.Context(), username, "", "")
	if errx != nil {
		var eresp identity.ErrorResponse
		if errors.As(errx, &eresp) {
			c.JSON(eresp.Status, gin.H{
				"error": eresp.Msg,
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get user: %v", errx),
		})
		return
	}

	// Compose and convert to json
	bp, err := a.server.bpManager.ComposeWithScope(blueprintYAML, scope)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Failed to compose blueprint with scope: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, bp)
}

// resolveBlueprintFromRequest resolves blueprint either from query parameter or request payload
func (a *RESTApiService) resolveBlueprintFromRequest(c *gin.Context, scope *blueprint.BlueprintScope) (*models.Blueprint, error) {
	blueprintName := c.Query("blueprint")

	// Check if request has body
	if c.Request.ContentLength > 0 {
		if blueprintName != "" {
			return nil, fmt.Errorf("cannot use both blueprint query parameter and request payload")
		}

		contentType := c.GetHeader("Content-Type")

		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read request body: %w", err)
		}

		var blueprintYAML []byte
		if strings.Contains(contentType, "text/yaml") || strings.Contains(contentType, "application/x-yaml") {
			blueprintYAML = body
		} else {
			return nil, fmt.Errorf("unsupported content type, expected text/yaml or application/x-yaml")
		}

		validationErrors := models.ValidateCustomBlueprint(blueprintYAML)
		if len(validationErrors) > 0 {
			return nil, fmt.Errorf("blueprint validation failed: %s", strings.Join(validationErrors, "; "))
		}

		return a.server.bpManager.ComposeWithScope(blueprintYAML, scope)
	} else {
		if blueprintName == "" {
			return nil, fmt.Errorf("blueprint query parameter is required when no payload is provided")
		}

		return a.server.bpManager.GetBlueprint(blueprintName, scope)
	}
}

// *** Workspaces

// GetWorkspaces handles the GET request for workspaces
// @Summary      List workspaces
// @Description  Get a list of workspaces, optionally filtered by username and/or blueprint
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        username   query   string  false  "Filter by username"
// @Param        blueprint  query   string  false  "Filter by blueprint name"
// @Success      200        {array}   models.WorkspaceResponse  "List of workspaces"
// @Failure      400        {object}  ErrorResponse      "Bad request - invalid parameters"
// @Failure      401        {object}  ErrorResponse      "Unauthorized"
// @Failure      500        {object}  ErrorResponse      "Internal server error"
// @Router       /api/v1/workspaces [get]
func (a *RESTApiService) GetWorkspaces(c *gin.Context) {
	username := c.Query("username")
	blueprint := c.Query("blueprint")

	// TODO: check if the user is a valid user first in identity
	// TODO: there could be inconsistencies, there could be workspaces of users that do not exist

	workspaces, err := workspace.GetWorkspaceInfo(a.server.helm, "", username, blueprint)
	if err != nil {
		errToJSONError(c, err)
		return
	}

	info := make([]models.WorkspaceInfo, 0, len(workspaces))
	for _, ws := range workspaces {
		info = append(info, ws)
	}

	c.JSON(http.StatusOK, info)
}

// GetWorkspace handles the GET request for a specific workspace
// @Summary      Get a specific workspace
// @Description  Get details of a workspace by name
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        name  path    string  true  "Workspace name"
// @Success      200   {object}  models.WorkspaceResponse  "Workspace details"
// @Failure      404   {object}  ErrorResponse      "Workspace not found"
// @Failure      409   {object}  ErrorResponse      "Multiple workspaces found with same name"
// @Failure      401   {object}  ErrorResponse      "Unauthorized"
// @Failure      500   {object}  ErrorResponse      "Internal server error"
// @Router       /api/v1/workspaces/{name} [get]
func (a *RESTApiService) GetWorkspace(c *gin.Context) {
	name := c.Param("name")

	// TODO: check if the user is a valid user first in identity
	// TODO: there could be inconsistencies, there could be workspaces of users that do not exist

	info, err := workspace.GetWorkspaceInfo(a.server.helm, name, "", "")
	if err != nil {
		errToJSONError(c, err)
		return
	}

	if len(info) == 0 {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Workspace not found: %s", name),
		})
		return
	}

	if len(info) > 1 {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("Multiple workspaces found: %s", name),
		})
		return
	}

	c.JSON(http.StatusOK, models.WorkspaceInfo{
		Name:      info[0].Name,
		Username:  info[0].Username,
		Blueprint: info[0].Blueprint,
		Deployed:  info[0].Deployed,
	})
}

// GetWorkspaceStatus handles the GET request for workspace status
// @Summary      Get workspace status
// @Description  Get the current status of a workspace including pod status and IP
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        name  path    string  true  "Workspace name"
// @Success      200   {object}  models.WorkspaceStatus  "Workspace status details"
// @Failure      400   {object}  ErrorResponse              "Bad request - invalid parameters"
// @Failure      404   {object}  ErrorResponse              "Workspace not found"
// @Failure      401   {object}  ErrorResponse              "Unauthorized"
// @Failure      500   {object}  ErrorResponse              "Internal server error"
// @Router       /api/v1/workspaces/{name}/status [get]
func (a *RESTApiService) GetWorkspaceStatus(c *gin.Context) {
	name := c.Param("name")
	status, err := workspace.GetWorkspaceStatus(c.Request.Context(), a.server.helm, name)
	if err != nil {
		errToJSONError(c, err)
		return
	}
	c.JSON(http.StatusOK, status)
}

// TemplateWorkspace renders workspace templates
// @Summary      Template workspace
// @Description  Generate Kubernetes manifests for a workspace without provisioning
// @Tags         workspaces
// @Accept       text/yaml,application/x-yaml
// @Produce      text/yaml
// @Security     BearerAuth
// @Param        username   query   string  true   "Username for scope resolution"
// @Param        blueprint  query   string  false  "Blueprint name (required if no payload)"
// @Param        blueprint  body    string  false  "Custom blueprint YAML (alternative to query parameter)"
// @Success      200        {string}  string       "Rendered Kubernetes manifests in YAML format"
// @Failure      400        {object}  ErrorResponse  "Bad request - missing parameters or validation failed"
// @Failure      404        {object}  ErrorResponse  "Blueprint not found"
// @Failure      415        {object}  ErrorResponse  "Unsupported media type"
// @Failure      401        {object}  ErrorResponse  "Unauthorized"
// @Failure      500        {object}  ErrorResponse  "Internal server error"
// @Router       /api/v1/workspaces/template [post]
func (a *RESTApiService) TemplateWorkspace(c *gin.Context) {
	username := c.Query("username")

	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "username query parameter is required",
		})
		return
	}

	scope, errx := a.server.GetBlueprintScope(c.Request.Context(), username, "", "")
	if errx != nil {
		var eresp identity.ErrorResponse
		if errors.As(errx, &eresp) {
			c.JSON(eresp.Status, gin.H{
				"error": eresp.Msg,
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get user: %v", errx),
		})
		return
	}

	blueprint, err := a.resolveBlueprintFromRequest(c, scope)
	if err != nil {
		if strings.Contains(err.Error(), "cannot use both") ||
			strings.Contains(err.Error(), "required when no payload") ||
			strings.Contains(err.Error(), "validation failed") {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": err.Error(),
			})
		} else if strings.Contains(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, gin.H{
				"error": err.Error(),
			})
		} else if strings.Contains(err.Error(), "unsupported content type") {
			c.JSON(http.StatusUnsupportedMediaType, gin.H{
				"error": err.Error(),
			})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": err.Error(),
			})
		}
		return
	}

	ws, err := workspace.NewWorkspace(blueprint, scope.User, a.server.helm)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to create workspace: %v", err),
		})
		return
	}

	renderedManifests, err := ws.Template(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to render workspace templates: %v", err),
		})
		return
	}

	c.Data(http.StatusOK, "application/x-yaml", []byte(renderedManifests))
}

// ProvisionWorkspace provisions a new workspace
// @Summary      Provision workspace
// @Description  Create and deploy a new workspace to Kubernetes
// @Tags         workspaces
// @Accept       text/yaml,application/x-yaml
// @Produce      json,application/x-ndjson
// @Security     BearerAuth
// @Param        username   query   string  true   "Username for scope resolution"
// @Param        blueprint  query   string  false  "Blueprint name (required if no payload)"
// @Param        timeout    query   int     false  "Timeout in seconds (default: 20)"
// @Param        stream     query   bool    false  "Enable streaming updates (default: false)"
// @Param        blueprint  body    string  false  "Custom blueprint YAML (alternative to query parameter)"
// @Success      200        {object}  StreamEventResponse     "Streaming events (when stream=true)"
// @Success      201        {object}  WorkspaceStatusResponse "Workspace status (when stream=false)"
// @Failure      400        {object}  ErrorResponse           "Bad request - missing parameters or validation failed"
// @Failure      404        {object}  ErrorResponse           "Blueprint not found"
// @Failure      415        {object}  ErrorResponse           "Unsupported media type"
// @Failure      401        {object}  ErrorResponse           "Unauthorized"
// @Failure      500        {object}  ErrorResponse           "Internal server error"
// @Router       /api/v1/workspaces [post]
func (a *RESTApiService) ProvisionWorkspace(c *gin.Context) {
	username := c.Query("username")
	stream := c.Query("stream") == "true"
	timeoutStr := c.Query("timeout")

	a.log.Debug().Msgf("ProvisionWorkspace called with username=%s, stream=%t, timeout=%s", username, stream, timeoutStr)

	timeout := 20
	if timeoutStr != "" {
		var err error
		timeout, err = strconv.Atoi(timeoutStr)
		if err != nil || timeout <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "Invalid timeout value",
			})
			return
		}
	}

	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "username query parameter is required",
		})
		return
	}

	scope, errx := a.server.GetBlueprintScope(c.Request.Context(), username, "", "")
	if errx != nil {
		var eresp identity.ErrorResponse
		if errors.As(errx, &eresp) {
			c.JSON(eresp.Status, gin.H{
				"error": eresp.Msg,
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to get user: %v", errx),
		})
		return
	}

	blueprint, err := a.resolveBlueprintFromRequest(c, scope)
	if err != nil {
		if strings.Contains(err.Error(), "cannot use both") ||
			strings.Contains(err.Error(), "required when no payload") ||
			strings.Contains(err.Error(), "validation failed") {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": err.Error(),
			})
		} else if strings.Contains(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, gin.H{
				"error": err.Error(),
			})
		} else if strings.Contains(err.Error(), "unsupported content type") {
			c.JSON(http.StatusUnsupportedMediaType, gin.H{
				"error": err.Error(),
			})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": err.Error(),
			})
		}
		return
	}

	ws, err := workspace.NewWorkspace(blueprint, scope.User, a.server.helm)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to create workspace: %v", err),
		})
		return
	}

	if stream {
		a.provisionWithStreaming(c, ws, timeout)
	} else {
		a.provisionSync(c, ws, timeout)
	}
}

// provisionWithStreaming handles workspace provisioning with streaming updates
func (a *RESTApiService) provisionWithStreaming(c *gin.Context, ws *workspace.Workspace, timeout int) {
	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Transfer-Encoding", "chunked")
	c.Header("Cache-Control", "no-cache")

	// Check if streaming is supported
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Streaming not supported",
		})
		return
	}

	c.Status(http.StatusOK)

	messages := make(chan models.StreamEvent, 100)

	done := make(chan *models.WorkspaceStatus)
	errorChan := make(chan error)

	go func() {
		defer close(done)
		defer close(errorChan)

		status, err := ws.Provision(c.Request.Context(), &workspace.ProvisionOptions{
			Timeout:  timeout,
			Messages: messages,
		})

		if err != nil {
			errorChan <- err
			return
		}

		done <- status
	}()

	// Stream events
	for {
		select {
		case <-c.Request.Context().Done():
			return

		case msg, ok := <-messages:
			if !ok {
				continue
			}

			event := gin.H{
				"type":       "event",
				"timestamp":  msg.Timestamp,
				"objectName": msg.ObjectName,
				"message":    msg.Message,
			}
			data, _ := json.Marshal(event)
			c.Writer.Write([]byte(fmt.Sprintf("%s\n", data)))
			flusher.Flush()

		case status := <-done:
			if status != nil {
				final := gin.H{
					"type":    "status",
					"status":  status.Status,
					"message": status.Message,
					"podIP":   status.PodIP,
				}
				data, _ := json.Marshal(final)
				c.Writer.Write([]byte(fmt.Sprintf("%s\n", data)))
				flusher.Flush()
			}
			return

		case err := <-errorChan:
			if err != nil {
				errEvent := gin.H{
					"type":    "status",
					"status":  "Error",
					"message": err.Error(),
				}
				data, _ := json.Marshal(errEvent)
				c.Writer.Write([]byte(fmt.Sprintf("%s\n", data)))
				flusher.Flush()
			}
			return
		}
	}
}

// provisionSync handles synchronous workspace provisioning
func (a *RESTApiService) provisionSync(c *gin.Context, ws *workspace.Workspace, timeout int) {
	status, err := ws.Provision(c.Request.Context(), &workspace.ProvisionOptions{
		Timeout:  timeout,
		Messages: nil,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to provision workspace: %v", err),
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"status":  status.Status,
		"message": status.Message,
		"podIP":   status.PodIP,
	})
}

func errToJSONError(c *gin.Context, err error) {
	if errors.Is(err, workspace.ErrWorkspaceNotFound) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("%v", err),
		})
		return
	}
	if errors.Is(err, workspace.ErrInvalidParameters) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("%v", err),
		})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{
		"error": fmt.Sprintf("%v", err),
	})
}
