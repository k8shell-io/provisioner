package models

import (
	"errors"
	"fmt"
	"time"
)

// WorkspaceInfo represents information about a workspace
type WorkspaceInfo struct {
	Name      string    `json:"name" example:"dev-user123"`
	Username  string    `json:"username" example:"dev-user"`
	Blueprint string    `json:"blueprint" example:"dev"`
	Deployed  time.Time `json:"deployed" example:"2025-08-05T10:30:00Z"`
}

// PodStatus represents the status of a workspace pod
type PodStatus struct {
	Created time.Time `json:"created" example:"2025-08-05T10:30:00Z"`
	Status  string    `json:"status" example:"Running"`
	Message string    `json:"message" example:"Workspace is running"`
}

// WorkspaceStatus represents the current status of a workspace
// It contains information about the workspace pod status and in addition
// the workspace-specific details such host, port and access key and TLS certificate.
type WorkspaceStatus struct {
	PodStatus
	Name      string `json:"name"`
	Host      string `json:"host"`
	PodIP     string `json:"podIP"`
	Port      int    `json:"port"`
	AccessKey string `json:"accessKey"`
	TLSCert   string `json:"tlsCert"`
	Splash    string `json:"splash"`
}

// StreamEvent represents a streaming event response
type StreamEvent struct {
	Type       string `json:"type" example:"event"`
	Timestamp  string `json:"timestamp,omitempty" example:"2025-08-05T10:30:00Z"`
	ObjectName string `json:"objectName,omitempty" example:"dev-user123"`
	Message    string `json:"message,omitempty" example:"Pod is starting"`
	Status     string `json:"status,omitempty" example:"Running"`
}

// ErrWorkspaceNotFound is returned when a workspace is not found
var ErrWorkspaceNotFound = errors.New("workspace not found")

// ErrInvalidParameters is returned when the provided parameters are invalid
var ErrInvalidParameters = errors.New("invalid parameters")

func (e StreamEvent) String() string {
	if e.Type == "event" {
		return fmt.Sprintf("[%s] [%-12s] %s",
			e.Timestamp, e.ObjectName, e.Message)
	}
	if e.Type == "status" {
		return fmt.Sprintf("[%s] [%-12s] %s: %s",
			e.Timestamp, e.ObjectName, e.Status, e.Message)
	}
	return ""
}
