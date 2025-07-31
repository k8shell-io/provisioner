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
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/k8shell-io/provisioner/internal/blueprint"
	"github.com/k8shell-io/provisioner/internal/log"
	"github.com/rs/zerolog"
	httpSwagger "github.com/swaggo/http-swagger"
)

// RESTApiService represents the REST API service for the K8Shell Provisioner server.
type RESTApiService struct {
	httpConfig HttpConfig
	bpManager  *blueprint.BlueprintManager
	log        *zerolog.Logger
}

// responseRecorder is a wrapper for http.ResponseWriter
// to capture the status code and response body.
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       bytes.Buffer
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
func NewRESTAPI(httpConfig HttpConfig, bpManager *blueprint.BlueprintManager) (*RESTApiService, error) {
	log := log.NewLogger("api")

	return &RESTApiService{
		httpConfig: httpConfig,
		log:        log,
		bpManager:  bpManager,
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
		expectedKey := a.httpConfig.APIKey

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
		a.log.Debug().Msgf("Response: status %d, body: %s", rec.statusCode, rec.body.String())
	})
}

// Initialize the router
func (a *RESTApiService) initializeRouter() *mux.Router {
	router := mux.NewRouter()

	// api router with API key middleware
	apiRouter := router.PathPrefix("/api/v1").Subrouter()
	apiRouter.Use(a.apiKeyMiddleware)
	apiRouter.Use(a.loggingMiddleware)

	// Define API endpoints
	apiRouter.HandleFunc("/blueprints", a.GetBlueprints).Methods(http.MethodGet)
	apiRouter.HandleFunc("/blueprints/{name}", a.GetBlueprint).Methods(http.MethodGet)
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
		Addr:    fmt.Sprintf(":%d", a.httpConfig.Port),
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
	a.log.Info().Msg("GetBlueprints called")
	blueprints := a.bpManager.ListBlueprintNames()
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
	a.log.Info().Msgf("GetBlueprint called for %s", name)

	blueprint, err := a.bpManager.GetBlueprint(name, blueprint.TestScope())
	if err != nil {
		http.Error(w, fmt.Sprintf("Blueprint not found: %s", name), http.StatusNotFound)
		return
	}

	data, err := json.Marshal(blueprint)
	if err != nil {
		http.Error(w, "Failed to marshal blueprint", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
