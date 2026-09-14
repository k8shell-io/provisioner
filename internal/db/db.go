// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

// Package db provides database access and management for the k8Shell
// Provisioner service. It provides persistence for org-scoped blueprint
// definitions, layered on top of the file-based blueprints loaded by
// internal/blueprint.
package db

import (
	"fmt"

	"github.com/k8shell-io/common/pkg/db"
	"github.com/k8shell-io/common/pkg/logger"
	"github.com/rs/zerolog"
)

// DB wraps the shared database implementation for the provisioner service.
type DB struct {
	db.DB
	log *zerolog.Logger
}

// NewDB creates a new DB for the provisioner service.
func NewDB(config db.DBConfig) (*DB, error) {
	d, err := db.NewDB(config, "provisioner")
	if err != nil {
		return nil, fmt.Errorf("create db: %w", err)
	}
	return &DB{
		DB:  *d,
		log: logger.NewLogger("db")}, nil
}
