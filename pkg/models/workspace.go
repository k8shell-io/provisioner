package models

import (
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
	Host      string `json:"host" example:"10.42.0.123"`
	Port      int    `json:"port" example:"2822"`
	AccessKey string `json:"accessKey" example:"abc123"`
	TLSCert   string `json:"tlsCert" example:"-----BEGIN CERTIFICATE-----\nMIID...IDAQAB\n-----END CERTIFICATE-----"`
}

// StreamEvent represents a streaming event response
type StreamEvent struct {
	Type       string `json:"type" example:"event"`
	Timestamp  string `json:"timestamp,omitempty" example:"2025-08-05T10:30:00Z"`
	ObjectName string `json:"objectName,omitempty" example:"dev-user123"`
	Message    string `json:"message,omitempty" example:"Pod is starting"`
	Status     string `json:"status,omitempty" example:"Running"`
}

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
