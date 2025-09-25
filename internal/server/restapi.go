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
	log "github.com/k8shell-io/common/logger"
	"github.com/k8shell-io/common/models"
	identity "github.com/k8shell-io/identity/pkg/client"
	"github.com/k8shell-io/provisioner/internal/blueprint"
	ws "github.com/k8shell-io/provisioner/internal/workspace"
	provModels "github.com/k8shell-io/provisioner/pkg/models"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert/yaml"
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

// Custom logger middleware
func (a *RESTApiService) customLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)

		status := c.Writer.Status()
		ip := c.ClientIP()
		method := c.Request.Method
		path := c.Request.URL.Path

		a.log.Info().
			Str("method", method).
			Int("status", status).
			Str("path", path).
			Str("ip", ip).
			Dur("duration", latency).
			Msg("request")
	}
}

// initializeRouter sets up all routes
func (a *RESTApiService) initializeRouter() {
	a.engine.Use(a.customLogger())

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
			workspaces.DELETE("/:name", a.DeleteWorkspace)
			workspaces.GET("/:name/status", a.GetWorkspaceStatus)
			workspaces.POST("/template", a.TemplateWorkspace)
			workspaces.POST("", a.ProvisionWorkspace)
		}
	}

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
func (a *RESTApiService) GetBlueprint(c *gin.Context) {
	name := c.Param("name")
	username := c.Query("username")

	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Username is required",
		})
		return
	}

	user, err := a.server.Identity.GetUser(c.Request.Context(), username)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Failed to get user: %v", err),
		})
		return
	}

	if !user.HasBlueprint(name) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Blueprint not found: %s", name),
		})
		return
	}

	scope, errx := a.server.GetBlueprintScope("", user, nil)
	if errx != nil {
		errToJSONError(c, errx)
		return
	}

	blueprint, err := a.server.bpManager.GetBlueprint(name, scope)
	if err != nil {
		errToJSONError(c, err)
		return
	}

	c.JSON(http.StatusOK, blueprint)
}

// GetRawBlueprint handles the GET request for a specific raw blueprint
func (a *RESTApiService) GetRawBlueprint(c *gin.Context) {
	name := c.Param("name")

	rawBp, err := a.server.bpManager.GetRawBlueprint(name)
	if err != nil {
		errToJSONError(c, err)
		return
	}

	c.JSON(http.StatusOK, rawBp)
}

// ComposeBlueprint handles the POST request to compose a blueprint using a k8shell file YAML
func (a *RESTApiService) ComposeBlueprint(c *gin.Context) {
	contentType := c.GetHeader("Content-Type")
	username := c.Query("username")

	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Username is required",
		})
		return
	}

	user, err := a.server.Identity.GetUser(c.Request.Context(), username)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Failed to get user: %v", err),
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

	if !strings.Contains(contentType, "text/yaml") && !strings.Contains(contentType, "application/x-yaml") {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{
			"error": "Unsupported content type, expected text/yaml or application/x-yaml",
		})
		return
	}

	var k8shellFile models.K8shellFile
	yaml.Unmarshal(body, &k8shellFile)
	customBlueprint, validationErrors := models.ValidateK8shellFile(k8shellFile)
	if len(validationErrors) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Blueprint validation failed: %s", strings.Join(validationErrors, "; ")),
		})
		return
	}

	if !user.HasBlueprint(customBlueprint.Template) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Blueprint template %s not found for user %s", customBlueprint.Template, user.Username),
		})
		return
	}

	// Get the user's blueprint scope
	scope, errx := a.server.GetBlueprintScope("noname", user, nil)
	if errx != nil {
		errToJSONError(c, errx)
		return
	}

	// Compose and convert to json
	bp, err := a.server.bpManager.ComposeWithScope(customBlueprint, scope)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Failed to compose blueprint with scope: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, bp)
}

// resolveBlueprintFromRequest resolves blueprint either from query parameter or request payload
func (a *RESTApiService) resolveBlueprintFromRequest(c *gin.Context,
	scope *blueprint.BlueprintScope) (*models.Blueprint, error) {
	blueprintName := c.Query("blueprint")

	if c.Request.ContentLength > 0 {
		if blueprintName != "" {
			return nil, fmt.Errorf("cannot use both blueprint query parameter and request payload")
		}

		contentType := c.GetHeader("Content-Type")

		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read request body: %w", err)
		}

		if !strings.Contains(contentType, "text/yaml") && !strings.Contains(contentType, "application/x-yaml") {
			return nil, fmt.Errorf("unsupported content type, expected text/yaml or application/x-yaml")
		}

		var k8shellFile models.K8shellFile
		yaml.Unmarshal(body, &k8shellFile)
		customBlueprint, validationErrors := models.ValidateK8shellFile(k8shellFile)
		if len(validationErrors) > 0 {
			return nil, fmt.Errorf("blueprint validation failed: %s", strings.Join(validationErrors, "; "))
		}

		if !scope.User.HasBlueprint(customBlueprint.Template) {
			return nil, fmt.Errorf("blueprint template %s not found for user %s", customBlueprint.Template, scope.User.Username)
		}

		return a.server.bpManager.ComposeWithScope(customBlueprint, scope)
	} else {
		if blueprintName == "" {
			return nil, fmt.Errorf("blueprint query parameter is required when no payload is provided")
		}

		bp, err := a.server.bpManager.GetBlueprint(blueprintName, scope)
		if err != nil {
			return nil, fmt.Errorf("failed to get blueprint: %w", err)
		}

		if !scope.User.HasBlueprint(blueprintName) {
			return nil, fmt.Errorf("blueprint %s not found for user %s", blueprintName, scope.User.Username)
		}

		return bp, nil
	}
}

// *** Workspaces

// GetWorkspaces handles the GET request for workspaces
func (a *RESTApiService) GetWorkspaces(c *gin.Context) {
	username := c.Query("username")
	blueprint := c.Query("blueprint")

	user, err := a.server.Identity.GetUser(c.Request.Context(), username)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Failed to get user: %v", err),
		})
		return
	}

	if blueprint == "" {
		blueprint, err = a.server.bpManager.GetDefaultUserBlueprint(user)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("No blueprint specified in userstr and no default blueprint found for user: %v", err),
			})
			return
		}
	}

	workspaces, err := ws.GetWorkspaceInfo(a.server.helm, "", username, blueprint)
	if err != nil {
		errToJSONError(c, err)
		return
	}

	info := make([]provModels.WorkspaceInfo, 0, len(workspaces))
	info = append(info, workspaces...)

	c.JSON(http.StatusOK, info)
}

// GetWorkspace handles the GET request for a specific workspace
func (a *RESTApiService) GetWorkspace(c *gin.Context) {
	name := c.Param("name")

	info, err := ws.GetWorkspaceInfo(a.server.helm, name, "", "")
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

	c.JSON(http.StatusOK, provModels.WorkspaceInfo{
		Name:      info[0].Name,
		Username:  info[0].Username,
		Blueprint: info[0].Blueprint,
		Deployed:  info[0].Deployed,
	})
}

// GetWorkspaceStatus handles the GET request for workspace status
func (a *RESTApiService) GetWorkspaceStatus(c *gin.Context) {
	name := c.Param("name")
	status, err := ws.GetWorkspaceStatus(c.Request.Context(), a.server.helm, name)
	if err != nil {
		errToJSONError(c, err)
		return
	}
	c.JSON(http.StatusOK, status)
}

// TemplateWorkspace renders workspace templates
func (a *RESTApiService) TemplateWorkspace(c *gin.Context) {
	username := c.Query("username")

	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "username query parameter is required",
		})
		return
	}

	user, err := a.server.Identity.GetUser(c.Request.Context(), username)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Failed to get user: %v", err),
		})
		return
	}

	scope, errx := a.server.GetBlueprintScope("noblueprint", user, nil)
	if errx != nil {
		errToJSONError(c, errx)
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
		} else if strings.Contains(err.Error(), "does not have access") {
			c.JSON(http.StatusForbidden, gin.H{
				"error": err.Error(),
			})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": err.Error(),
			})
		}
		return
	}

	ws, err := ws.NewWorkspace(blueprint, user, a.server.helm, a.server.Identity)
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
func (a *RESTApiService) ProvisionWorkspace(c *gin.Context) {
	userstrParam := c.Query("userstr")
	stream := c.Query("stream") == "true"
	timeoutStr := c.Query("timeout")

	a.log.Debug().Msgf("ProvisionWorkspace called with userstr=%s, stream=%t, timeout=%s",
		userstrParam, stream, timeoutStr)

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

	if userstrParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "userstr query parameter is required",
		})
		return
	}

	userstr, err := models.NewUserStr(userstrParam)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Invalid userstr format: %v", err),
		})
		return
	}

	user, err := a.server.Identity.GetUser(c.Request.Context(), userstr.Username)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("Failed to get user: %v", err),
		})
		return
	}

	bpName := userstr.Blueprint
	if bpName == "" {
		bpName, err = a.server.bpManager.GetDefaultUserBlueprint(user)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("No blueprint specified in userstr and no default blueprint found for user: %v", err),
			})
			return
		}
	}

	var blueprintObj *models.Blueprint

	if userstr.HasCustomBlueprint {
		customBlueprint, err := a.server.Identity.GetBlueprintByUserStr(c.Request.Context(), userstrParam)
		if err != nil {
			// Check if it's a "not found" error
			var eresp identity.ErrorResponse
			if errors.As(err, &eresp) && eresp.Status == http.StatusNotFound {
				c.JSON(http.StatusBadRequest, gin.H{
					"error": fmt.Sprintf("No blueprint was provided, and no default blueprint is configured for user %q.", userstr.Username),
				})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": fmt.Sprintf("Failed to lookup custom blueprint: %v", err),
			})
			return
		}

		if customBlueprint == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("No blueprint was provided, and no default blueprint found for user %q.", userstr.Username),
			})
			return
		}

		if !user.HasBlueprint(customBlueprint.Template) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("Access denied: user %q is not authorized to use blueprint's template %q.", userstr.Username, customBlueprint.Template),
			})
			return
		}

		scope, errx := a.server.GetBlueprintScope("", user, &customBlueprint.Metadata)
		if errx != nil {
			errToJSONError(c, errx)
			return
		}

		blueprintObj, err = a.server.bpManager.ComposeWithScope(customBlueprint, scope)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("Failed to compose blueprint: %v", err),
			})
			return
		}

		user = scope.User
	} else {
		scope, errx := a.server.GetBlueprintScope(bpName, user, nil)
		if errx != nil {
			errToJSONError(c, errx)
			return
		}

		if !user.HasBlueprint(bpName) {
			c.JSON(http.StatusForbidden, gin.H{
				"error": fmt.Sprintf("Access denied: user %q is not authorized to use blueprint %q.",
					userstr.Username, bpName),
			})
			return
		}

		blueprintObj, err = a.server.bpManager.GetBlueprint(bpName, scope)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error": fmt.Sprintf("Blueprint %q not found.", userstr.Blueprint),
			})
			return
		}

		if blueprintObj.IsTemplate {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("Blueprint %q is a template and cannot be used to provision a workspace.",
					userstr.Blueprint),
			})
			return
		}
	}

	workspace, err := ws.NewWorkspace(blueprintObj, user, a.server.helm, a.server.Identity)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to create workspace: %v", err),
		})
		return
	}

	if stream {
		a.provisionWithStreaming(c, workspace, timeout)
	} else {
		a.provisionSync(c, workspace, timeout)
	}
}

// provisionWithStreaming handles workspace provisioning with streaming updates
func (a *RESTApiService) provisionWithStreaming(c *gin.Context, workspace *ws.Workspace, timeout int) {
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

	pushInternalEvent := func(status string, message string) {
		msg := gin.H{
			"type":       "status",
			"timestamp":  time.Now().Format("2006-01-02 15:04:05"),
			"objectName": workspace.Name(),
			"status":     status,
			"message":    message,
		}
		data, _ := json.Marshal(msg)
		fmt.Fprintf(c.Writer, "%s\n", data)
		flusher.Flush()
	}

	pushStreamEvent := func(event provModels.StreamEvent) {
		msg := gin.H{
			"type":       "event",
			"timestamp":  event.Timestamp,
			"objectName": event.ObjectName,
			"message":    event.Message,
		}
		data, _ := json.Marshal(msg)
		fmt.Fprintf(c.Writer, "%s\n", data)
		flusher.Flush()
	}

	c.Status(http.StatusOK)
	pushInternalEvent("Starting", "Provisioning started")

	messages := make(chan provModels.StreamEvent, 100)

	done := make(chan *provModels.PodStatus)
	errorChan := make(chan error)

	// Provision the workspace
	go func() {
		defer close(done)
		defer close(errorChan)

		status, err := workspace.Provision(c.Request.Context(), &ws.ProvisionOptions{
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
			pushStreamEvent(msg)

		case status := <-done:
			if status != nil {
				pushInternalEvent(status.Status, status.Message)
			}
			return

		case err := <-errorChan:
			if err != nil {
				pushInternalEvent("Error", err.Error())
			}
			return
		}
	}
}

// provisionSync handles synchronous workspace provisioning
func (a *RESTApiService) provisionSync(c *gin.Context, workspace *ws.Workspace, timeout int) {
	status, err := workspace.Provision(c.Request.Context(), &ws.ProvisionOptions{
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
	})
}

func errToJSONError(c *gin.Context, err error) {
	var eresp identity.ErrorResponse
	if errors.As(err, &eresp) {
		c.JSON(eresp.Status, gin.H{
			"error": eresp.Msg,
		})
		return
	}

	if errors.Is(err, provModels.ErrWorkspaceNotFound) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("%v", err),
		})
		return
	}
	if errors.Is(err, provModels.ErrInvalidParameters) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("%v", err),
		})
		return
	}
	if errors.Is(err, blueprint.ErrBlueprintNotFound) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("%v", err),
		})
		return
	}

	c.JSON(http.StatusInternalServerError, gin.H{
		"error": fmt.Sprintf("%v", err),
	})
}

func (a *RESTApiService) DeleteWorkspace(c *gin.Context) {
	name := c.Param("name")

	w, err := ws.NewWorkspaceFromHelmRelease(c.Request.Context(), name, a.server.helm, a.server.Identity)
	if err != nil {
		errToJSONError(c, err)
		return
	}

	asyncCtx := context.Background()

	go func() {
		time.Sleep(2 * time.Second)
		a.log.Info().Msgf("Starting async deletion of workspace %s", name)
		err := w.Uninstall(asyncCtx, time.Duration(10)*time.Second, false)
		if err != nil {
			a.log.Error().Err(err).Msgf("Failed to delete workspace %s", name)
		} else {
			a.log.Info().Msgf("Successfully deleted workspace %s", name)
		}
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"message": fmt.Sprintf("Request to delete the workspace %s was submitted", name),
	})
}
