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

// WorkspaceStatus represents the current status of a workspace
type WorkspaceStatus struct {
	Created time.Time `json:"created" example:"2025-08-05T10:30:00Z"`
	Status  string    `json:"status" example:"Running"`
	Message string    `json:"message" example:"Workspace is running"`
	PodIP   string    `json:"podIP" example:"10.42.0.123"`
}

// StreamEvent represents a streaming event response
type StreamEvent struct {
	Type       string `json:"type" example:"event"`
	Timestamp  string `json:"timestamp,omitempty" example:"2025-08-05T10:30:00Z"`
	ObjectName string `json:"objectName,omitempty" example:"dev-user123"`
	Message    string `json:"message,omitempty" example:"Pod is starting"`
	Status     string `json:"status,omitempty" example:"Running"`
	PodIP      string `json:"podIP,omitempty" example:"10.42.0.123"`
}

func (e StreamEvent) String() string {
	if e.Type == "event" {
		return fmt.Sprintf("[%s] [%-12s] %s",
			e.Timestamp, e.ObjectName, e.Message)
	}
	if e.Type == "status" {
		return fmt.Sprintf("[%s] %s: %s",
			e.Timestamp, e.Status, e.Message)
	}
	return ""
}
