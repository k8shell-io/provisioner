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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	identity "github.com/k8shell-io/identity/pkg/client"
	"github.com/k8shell-io/provisioner/internal/blueprint"
	"github.com/k8shell-io/provisioner/internal/log"
	"github.com/k8shell-io/provisioner/internal/workspace"
	"github.com/k8shell-io/provisioner/pkg/models"
	"github.com/rs/zerolog"
	httpSwagger "github.com/swaggo/http-swagger"
)

// RESTApiService represents the REST API service for the K8Shell Provisioner server.
type RESTApiService struct {
	server *Server
	log    *zerolog.Logger
}

// responseRecorder is a wrapper for http.ResponseWriter
// to capture the status code and response body.
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       bytes.Buffer
}

type BlueprintComposeRequest struct {
	Blueprint models.CustomBlueprint   `json:"blueprint"`
	Scope     blueprint.BlueprintScope `json:"scope"`
}

// WriteHeader captures the status code and forwards it to the original ResponseWriter
func (rec *responseRecorder) WriteHeader(code int) {
	rec.statusCode = code
	rec.ResponseWriter.WriteHeader(code)
}

// Write captures the response body and writes it to the original ResponseWriter
func (rec *responseRecorder) Write(data []byte) (int, error) {
	rec.body.Write(data)
	return rec.ResponseWriter.Write(data)
}

// NewRESTAPI creates a new REST API service
func NewRESTAPI(server *Server) (*RESTApiService, error) {
	log := log.NewLogger("api")

	return &RESTApiService{
		server: server,
		log:    log,
	}, nil
}

// apiKeyMiddleware checks for the presence of a valid API key in the request header
// and allows access to the API endpoints only if the key matches the configured one.
func (a *RESTApiService) apiKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		const prefix = "Bearer "

		if !strings.HasPrefix(authHeader, prefix) {
			http.Error(w, "Unauthorized: missing or malformed Authorization header", http.StatusUnauthorized)
			return
		}

		providedKey := strings.TrimPrefix(authHeader, prefix)
		expectedKey := a.server.config.Http.APIKey

		if providedKey != expectedKey {
			http.Error(w, "Unauthorized: invalid API key", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Middleware to log requests and responses
func (a *RESTApiService) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.log.Debug().Msgf("Request: method %s, path %s", r.Method, r.URL.Path)
		rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.statusCode >= 400 {
			a.log.Error().Msgf("Response: status %d, %s", rec.statusCode, rec.body.String())
		} else {
			a.log.Debug().Msgf("Response: status %d, body: %s", rec.statusCode, rec.body.String()[:100])
		}
	})
}

// Initialize the router
func (a *RESTApiService) initializeRouter() *mux.Router {
	router := mux.NewRouter()

	// api router with API key middleware
	apiRouter := router.PathPrefix("/api/v1").Subrouter()
	apiRouter.Use(a.apiKeyMiddleware)
	// apiRouter.Use(a.loggingMiddleware)

	// Blueprint routes
	apiRouter.HandleFunc("/blueprints", a.GetBlueprints).Methods(http.MethodGet)
	apiRouter.HandleFunc("/blueprints/{name}", a.GetBlueprint).Methods(http.MethodGet)
	apiRouter.HandleFunc("/blueprints/{name}/raw", a.GetRawBlueprint).Methods(http.MethodGet)
	apiRouter.HandleFunc("/blueprints/compose", a.ComposeBlueprint).Methods(http.MethodPost)

	// Workspace routes
	apiRouter.HandleFunc("/workspaces/template", a.TemplateWorkspace).Methods(http.MethodPost)
	apiRouter.HandleFunc("/workspaces", a.ProvisionWorkspace).Methods(http.MethodPost)

	a.logRoutes(router)

	router.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.log.Debug().Msgf("404 Not Found: %s %s", r.Method, r.URL.Path)
		http.Error(w, "404 route not found", http.StatusNotFound)
	})

	router.PathPrefix("/swagger/").Handler(httpSwagger.WrapHandler)

	return router
}

func (a *RESTApiService) Serve(ctx context.Context) {
	server := &http.Server{
		Handler: a.initializeRouter(),
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

// logRoutes logs all registered routes in the router
func (a *RESTApiService) logRoutes(router *mux.Router) {
	err := router.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		path, err := route.GetPathTemplate()
		if err != nil {
			path = "<undefined>"
		}

		methods, err := route.GetMethods()
		if err != nil {
			methods = []string{"<any>"}
		}

		a.log.Debug().Msgf("Route: %s Methods: %v", path, methods)
		return nil
	})

	if err != nil {
		a.log.Error().Msgf("Error walking routes: %v", err)
	}
}

// GetBlueprints handles the GET request for blueprints
func (a *RESTApiService) GetBlueprints(w http.ResponseWriter, r *http.Request) {
	blueprints := a.server.bpManager.ListBlueprintNames()
	if len(blueprints) == 0 {
		http.Error(w, "No blueprints found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	response := map[string]interface{}{}
	for _, bp := range blueprints {
		response[bp] = map[string]string{"name": bp, "url": fmt.Sprintf("/api/v1/blueprints/%s", bp)}
	}

	data, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "Failed to marshal response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// GetBlueprint handles the GET request for a specific blueprint
func (a *RESTApiService) GetBlueprint(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]

	username := r.URL.Query().Get("username")
	if username == "" {
		http.Error(w, "Username is required", http.StatusBadRequest)
		return
	}

	// Get the user's blueprint scope
	scope, errx := a.server.GetBlueprintScope(r.Context(), username, "", "")
	if errx != nil {
		var eresp identity.ErrorResponse
		if errors.As(errx, &eresp) {
			http.Error(w, eresp.Msg, eresp.Status)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to get user: %v", errx), http.StatusInternalServerError)
		return
	}

	var data []byte
	blueprint, err := a.server.bpManager.GetBlueprint(name, scope)
	if err != nil {
		http.Error(w, fmt.Sprintf("Blueprint not found: %s", name), http.StatusNotFound)
		return
	}

	data, err = json.Marshal(blueprint)
	if err != nil {
		http.Error(w, "Failed to marshal blueprint", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// GetBlueprint handles the GET request for a specific blueprint
func (a *RESTApiService) GetRawBlueprint(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]

	var data []byte
	rawBp, err := a.server.bpManager.GetRawBlueprint(name)
	if err != nil {
		http.Error(w, fmt.Sprintf("Raw blueprint not found: %s", name), http.StatusNotFound)
		return
	}

	data, err = json.Marshal(rawBp)
	if err != nil {
		http.Error(w, "Failed to marshal raw blueprint", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// ComposeBlueprint handles the POST request to compose a blueprint
func (a *RESTApiService) ComposeBlueprint(w http.ResponseWriter, r *http.Request) {
	contentType := r.Header.Get("Content-Type")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	username := r.URL.Query().Get("username")
	if username == "" {
		http.Error(w, "Username is required", http.StatusBadRequest)
		return
	}

	var blueprintYAML []byte
	if strings.Contains(contentType, "text/yaml") || strings.Contains(contentType, "application/x-yaml") {
		blueprintYAML = body
	} else {
		http.Error(w, "Unsupported content type, expected text/yaml or application/x-yaml",
			http.StatusUnsupportedMediaType)
		return
	}

	// Validate the custom blueprint YAML
	validationErrors := models.ValidateCustomBlueprint(blueprintYAML)
	if len(validationErrors) > 0 {
		http.Error(w, fmt.Sprintf("Blueprint validation failed: %s", strings.Join(validationErrors, "; ")),
			http.StatusBadRequest)
		return
	}

	// Get the user's blueprint scope
	scope, errx := a.server.GetBlueprintScope(r.Context(), username, "", "")
	if errx != nil {
		var eresp identity.ErrorResponse
		if errors.As(errx, &eresp) {
			http.Error(w, eresp.Msg, eresp.Status)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to get user: %v", errx), http.StatusInternalServerError)
		return
	}

	// compose and convert to json
	bp, err := a.server.bpManager.ComposeWithScope(blueprintYAML, scope)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to compose blueprint with scope: %v", err), http.StatusBadRequest)
		return
	}

	var composedJSON []byte
	composedJSON, err = json.Marshal(bp)
	if err != nil {
		http.Error(w, "Failed to process composed blueprint with scope", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(composedJSON)

}

// TemplateWorkspace handles POST /api/v1/workspaces/template
func (a *RESTApiService) TemplateWorkspace(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	blueprintName := r.URL.Query().Get("blueprint")

	if username == "" {
		http.Error(w, "username query parameter is required", http.StatusBadRequest)
		return
	}

	if blueprintName == "" {
		http.Error(w, "blueprint query parameter is required", http.StatusBadRequest)
		return
	}

	// Get the user's blueprint scope
	scope, errx := a.server.GetBlueprintScope(r.Context(), username, "", "")
	if errx != nil {
		var eresp identity.ErrorResponse
		if errors.As(errx, &eresp) {
			http.Error(w, eresp.Msg, eresp.Status)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to get user: %v", errx), http.StatusInternalServerError)
		return
	}

	blueprint, err := a.server.bpManager.GetBlueprint(blueprintName, scope)
	if err != nil {
		http.Error(w, fmt.Sprintf("Blueprint not found: %s", blueprintName), http.StatusNotFound)
		return
	}

	ws, err := workspace.NewWorkspace(blueprint, scope.User, a.server.helm)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create workspace: %v", err), http.StatusInternalServerError)
		return
	}

	renderedManifests, err := ws.Template(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to render workspace templates: %v", err), http.StatusInternalServerError)
		return
	}

	// Return rendered YAML manifests
	w.Header().Set("Content-Type", "application/x-yaml")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(renderedManifests))
}

// ProvisionWorkspace handles POST /api/v1/workspaces
func (a *RESTApiService) ProvisionWorkspace(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("username")
	blueprintName := r.URL.Query().Get("blueprint")
	stream := r.URL.Query().Get("stream") == "true"

	if username == "" {
		http.Error(w, "username query parameter is required", http.StatusBadRequest)
		return
	}

	if blueprintName == "" {
		http.Error(w, "blueprint query parameter is required", http.StatusBadRequest)
		return
	}

	scope, errx := a.server.GetBlueprintScope(r.Context(), username, "", "")
	if errx != nil {
		var eresp identity.ErrorResponse
		if errors.As(errx, &eresp) {
			http.Error(w, eresp.Msg, eresp.Status)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to get user: %v", errx), http.StatusInternalServerError)
		return
	}

	blueprint, err := a.server.bpManager.GetBlueprint(blueprintName, scope)
	if err != nil {
		http.Error(w, fmt.Sprintf("Blueprint not found: %s", blueprintName), http.StatusNotFound)
		return
	}

	ws, err := workspace.NewWorkspace(blueprint, scope.User, a.server.helm)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create workspace: %v", err), http.StatusInternalServerError)
		return
	}

	if stream {
		a.provisionWithStreaming(w, r, ws)
	} else {
		a.provisionSync(w, r, ws)
	}
}

// provisionWithStreaming handles workspace provisioning with streaming updates
func (a *RESTApiService) provisionWithStreaming(w http.ResponseWriter, r *http.Request, ws *workspace.Workspace) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("Cache-Control", "no-cache")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)

	messages := make(chan workspace.EventMessage, 100)
	initial := map[string]interface{}{
		"type":    "started",
		"message": "Starting workspace provisioning...",
	}
	data, _ := json.Marshal(initial)
	fmt.Fprintf(w, "%s\n", data)
	flusher.Flush()

	done := make(chan *workspace.WorkspaceStatus)
	errorChan := make(chan error)

	go func() {
		defer close(done)
		defer close(errorChan)

		status, err := ws.Provision(r.Context(), &workspace.ProvisionOptions{
			Timeout:  20 * time.Second,
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
		case <-r.Context().Done():
			return

		case msg, ok := <-messages:
			if !ok {
				continue
			}

			event := map[string]interface{}{
				"type":       "event",
				"timestamp":  msg.Timestamp,
				"objectName": msg.ObjectName,
				"message":    msg.Message,
			}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "%s\n", data)
			flusher.Flush()

		case status := <-done:
			if status != nil {
				final := map[string]interface{}{
					"type":    "complete",
					"status":  status.Status,
					"message": status.Message,
					"podIP":   status.PodIP,
				}
				data, _ := json.Marshal(final)
				fmt.Fprintf(w, "%s\n", data)
				flusher.Flush()
			}
			return

		case err := <-errorChan:
			if err != nil {
				errEvent := map[string]interface{}{
					"type":  "error",
					"error": err.Error(),
				}
				data, _ := json.Marshal(errEvent)
				fmt.Fprintf(w, "%s\n", data)
				flusher.Flush()
			}
			return
		}
	}
}

// provisionSync handles synchronous workspace provisioning
func (a *RESTApiService) provisionSync(w http.ResponseWriter, r *http.Request, ws *workspace.Workspace) {
	status, err := ws.Provision(r.Context(), &workspace.ProvisionOptions{
		Timeout:  20 * time.Second,
		Messages: nil,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to provision workspace: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	response := map[string]interface{}{
		"status":  status.Status,
		"message": status.Message,
		"podIP":   status.PodIP,
	}

	data, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "Failed to marshal response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	_, _ = w.Write(data)
}
