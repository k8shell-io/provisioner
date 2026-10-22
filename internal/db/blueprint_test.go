// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package db

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	commondb "github.com/k8shell-io/common/pkg/db"
)

// newIntegrationDB connects to PROVISIONER_TEST_DATABASE_URL and recreates
// the provisioner schema (plus a minimal identity.organizations stub for
// its FK). Tests using it are skipped when the variable is unset. The
// database it points at is wiped, so never point it at a real one.
func newIntegrationDB(t *testing.T) *DB {
	t.Helper()
	url := os.Getenv("PROVISIONER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PROVISIONER_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	schema, err := os.ReadFile("../../db/migrations/000001_schema.up.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS provisioner CASCADE`,
		`CREATE SCHEMA IF NOT EXISTS identity`,
		`CREATE TABLE IF NOT EXISTS identity.organizations (name varchar PRIMARY KEY)`,
		`INSERT INTO identity.organizations (name) VALUES ('acme') ON CONFLICT DO NOTHING`,
		string(schema),
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	return &DB{DB: commondb.DB{Pool: pool}}
}

func TestUpdateBlueprintCompareAndSwap(t *testing.T) {
	d := newIntegrationDB(t)

	v1 := []byte("name: dev\nimage: a:1\n")
	v2 := []byte("name: dev\nimage: a:2\n")
	v3 := []byte("name: dev\nimage: a:3\n")
	if _, err := d.CreateBlueprint("acme", "dev", "d", v1, false); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Matching expectation: applied.
	bp, err := d.UpdateBlueprint("acme", "dev", nil, v2, false, v1)
	if err != nil {
		t.Fatalf("CAS with current yaml: %v", err)
	}
	if string(bp.YAML) != string(v2) {
		t.Fatalf("expected yaml %q, got %q", v2, bp.YAML)
	}

	// Stale expectation: rejected, row unchanged.
	if _, err := d.UpdateBlueprint("acme", "dev", nil, v3, false, v1); !errors.Is(err, ErrBlueprintConflict) {
		t.Fatalf("CAS with stale yaml: expected ErrBlueprintConflict, got %v", err)
	}
	got, err := d.GetBlueprint("acme", "dev")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.YAML) != string(v2) {
		t.Fatalf("stale CAS modified the row: %q", got.YAML)
	}

	// No expectation: unconditional, as before.
	if _, err := d.UpdateBlueprint("acme", "dev", nil, v3, false, nil); err != nil {
		t.Fatalf("unconditional update: %v", err)
	}

	// Missing row: not found, with or without an expectation.
	for _, expected := range [][]byte{nil, v1} {
		if _, err := d.UpdateBlueprint("acme", "missing", nil, v1, false, expected); !errors.Is(err, ErrBlueprintNotFound) {
			t.Fatalf("missing row (expected=%q): expected ErrBlueprintNotFound, got %v", expected, err)
		}
	}
}

// TestUpdateBlueprintCompareAndSwapRace starts many writers from the same
// base and checks that exactly one of them wins.
func TestUpdateBlueprintCompareAndSwapRace(t *testing.T) {
	d := newIntegrationDB(t)

	base := []byte("name: race\nimage: base\n")
	if _, err := d.CreateBlueprint("acme", "race", "r", base, false); err != nil {
		t.Fatalf("create: %v", err)
	}

	const writers = 16
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			next := []byte("name: race\nimage: w" + string(rune('a'+i)) + "\n")
			_, errs[i] = d.UpdateBlueprint("acme", "race", nil, next, false, base)
		}(i)
	}
	wg.Wait()

	wins := 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrBlueprintConflict):
		default:
			t.Errorf("writer %d: unexpected error %v", i, err)
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 winning writer, got %d", wins)
	}
}
