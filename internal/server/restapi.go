// Copyright 2025 the k8Shell authors

package server

// @title        K8shell Provisioner API
// @version      1.1
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

type BlueprintComposeRequest struct {
	Blueprint models.CustomBlueprint   `json:"blueprint"`
	Scope     blueprint.BlueprintScope `json:"scope"`
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

// Updated TemplateWorkspace using the helper
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

// ProvisionWorkspace handles the POST request to provision a workspace
func (a *RESTApiService) ProvisionWorkspace(c *gin.Context) {
	username := c.Query("username")
	stream := c.Query("stream") == "true"
	timeoutStr := c.Query("timeout")

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

	messages := make(chan workspace.EventMessage, 100)

	// Send initial message
	initial := gin.H{
		"type":    "started",
		"message": "Starting workspace provisioning...",
	}
	data, _ := json.Marshal(initial)
	c.Writer.Write([]byte(fmt.Sprintf("%s\n", data)))
	flusher.Flush()

	done := make(chan *workspace.WorkspaceStatus)
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
					"type":  "error",
					"error": err.Error(),
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
